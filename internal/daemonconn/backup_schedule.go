package daemonconn

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// BackupSchedule reads the runtime overrides and the schedule in force.
func (c *Connection) BackupSchedule(ctx context.Context) (api.BackupSchedule, error) {
	var response *http.Response
	schedule, err := c.apiWithResponse(&response).GetBackupSchedule(ctx)
	if err != nil {
		return api.BackupSchedule{}, err
	}
	if err := validateBackupScheduleResponse(*schedule, response.Header.Get("ETag")); err != nil {
		return api.BackupSchedule{}, err
	}
	return *schedule, nil
}

// SetBackupSchedule changes overrides at the expected settings revision.
func (c *Connection) SetBackupSchedule(ctx context.Context, revision int64, request api.SetBackupScheduleRequest) (api.BackupSchedule, error) {
	if revision < 1 {
		return api.BackupSchedule{}, errors.New("invalid settings revision")
	}
	var response *http.Response
	schedule, err := c.apiWithResponse(&response).SetBackupSchedule(ctx, &apiclient.SetBackupScheduleRequestOptions{
		Header: &apiclient.SetBackupScheduleHeaders{IfMatch: revisionIfMatch(revision)},
		Body:   &request,
	})
	if err != nil {
		return api.BackupSchedule{}, mutationRequestError(response, err)
	}
	if err := validateBackupScheduleResponse(*schedule, response.Header.Get("ETag")); err != nil {
		return api.BackupSchedule{}, &responseDecodeError{err: err}
	}
	return *schedule, nil
}

func validateBackupScheduleResponse(schedule api.BackupSchedule, etag string) error {
	if schedule.Revision < 1 || schedule.EffectiveIntervalSeconds < 0 || schedule.EffectiveKeepLast < 0 {
		return errors.New("backup schedule response is invalid")
	}
	return validatePhotoETag(etag, schedule.Revision)
}

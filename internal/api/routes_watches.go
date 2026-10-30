package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/config"
)

func registerWatchRoutes(api huma.API, d Deps) {
	type output struct {
		Body WatchedInboxList
	}
	huma.Register(api, huma.Operation{
		OperationID: "listWatchedInboxes", Method: http.MethodGet, Path: "/api/v1/watches",
		Summary: "List effective watched-inbox configuration and runner status",
	}, func(ctx context.Context, _ *struct{}) (*output, error) {
		watches := slices.Clone(d.Cfg.Watches)
		slices.SortFunc(watches, func(a, b config.WatchConfig) int {
			return cmp.Compare(a.Name, b.Name)
		})
		jobsByName := make(map[string]Job)
		if d.Jobs != nil {
			for _, snapshot := range d.Jobs.Snapshot() {
				jobsByName[snapshot.Name] = observableJob(snapshot)
			}
		}
		out := &output{Body: WatchedInboxList{Items: make([]WatchedInbox, 0, len(watches))}}
		for _, watch := range watches {
			item := WatchedInbox{
				Name: watch.Name, Source: watch.Source, Destination: watch.Destination,
				CaptureOnChange: watch.CaptureOnChange,
				SettleTime:      watch.SettleTime.Std().String(),
				MinimumAge:      watch.MinimumAge.Std().String(),
				ScanInterval:    watch.ScanInterval.Std().String(),
				Exclude:         append([]string{}, watch.Exclude...),
				ExcludePatterns: append([]string{}, watch.ExcludePatterns...),
			}
			item.Processing = watchProcessingStatus(ctx, d, watch.Profile)
			if job, ok := jobsByName["watch:"+watch.Name]; ok {
				item.Job = &job
			}
			out.Body.Items = append(out.Body.Items, item)
		}
		return out, nil
	})
}

func watchProcessingStatus(ctx context.Context, d Deps, profile string) *WatchProcessingStatus {
	if profile == "" {
		return nil
	}
	status := &WatchProcessingStatus{Profile: profile}
	if d.Processing == nil {
		status.Error = "Processing status unavailable"
		return status
	}
	state, err := d.Processing.RenditionConsentState(ctx, profile)
	if err != nil {
		status.Error = "Consent status unavailable; inspect with the Docbank CLI for details"
	} else {
		status.ConsentState = state
	}
	return status
}

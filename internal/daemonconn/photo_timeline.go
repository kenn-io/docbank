package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/query"
)

// PhotoTimeline counts the photo assets that request selects by recorded
// capture day. It refuses a response whose day counts do not add up.
func (c *Connection) PhotoTimeline(ctx context.Context, request api.PhotoTimelineRequest) (api.PhotoTimeline, error) {
	if _, err := query.Parse(request.Query); err != nil {
		return api.PhotoTimeline{}, fmt.Errorf("photo timeline query: %w", err)
	}
	response, err := c.API().GetPhotoTimeline(ctx, &apiclient.GetPhotoTimelineRequestOptions{Body: &request})
	if err != nil {
		return api.PhotoTimeline{}, err
	}
	if err := validatePhotoTimeline(*response); err != nil {
		return api.PhotoTimeline{}, err
	}
	return *response, nil
}

func validatePhotoTimeline(timeline api.PhotoTimeline) error {
	if len(timeline.Facets) != 1 || timeline.Facets[0].Dimension != "capture_day" {
		return errors.New("photo timeline must contain one capture_day facet")
	}
	facet := timeline.Facets[0]
	if !facet.Available {
		if facet.Reason == "" || len(facet.Values) != 0 || facet.Total != nil || facet.Missing != nil || facet.Other != nil {
			return errors.New("unavailable photo timeline reports counts")
		}
		return nil
	}
	if facet.Total == nil || facet.Missing == nil || facet.Other == nil || *facet.Missing < 0 || *facet.Other != 0 {
		return errors.New("photo timeline lacks complete counts")
	}
	sum := *facet.Missing
	previous := ""
	for _, value := range facet.Values {
		day, err := time.Parse(time.DateOnly, value.Key)
		if err != nil || day.Format(time.DateOnly) != value.Key || value.Count < 1 {
			return fmt.Errorf("photo timeline day %q is not a counted calendar date", value.Key)
		}
		if previous != "" && value.Key >= previous {
			return fmt.Errorf("photo timeline day %q is out of descending order", value.Key)
		}
		previous = value.Key
		sum += value.Count
	}
	if sum != *facet.Total {
		return errors.New("photo timeline day counts do not add up to its total")
	}
	return nil
}

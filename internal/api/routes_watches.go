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
	}, func(_ context.Context, _ *struct{}) (*output, error) {
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
				SettleTime:   watch.SettleTime.Std().String(),
				ScanInterval: watch.ScanInterval.Std().String(),
				Exclude:      append([]string{}, watch.Exclude...),
			}
			if job, ok := jobsByName["watch:"+watch.Name]; ok {
				item.Job = &job
			}
			out.Body.Items = append(out.Body.Items, item)
		}
		return out, nil
	})
}

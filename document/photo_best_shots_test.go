package document

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPlanPhotoBestShots(t *testing.T) {
	a := PhotoBestShotFrame{AssetID: "a", Signals: map[string]float64{"focus": 0.8, "aesthetics": 0, "framing": 0.1}}
	b := PhotoBestShotFrame{AssetID: "b", Signals: map[string]float64{"focus": 0.4, "aesthetics": 0, "framing": 0.9, "eyes": 1}}
	unknown := PhotoBestShotFrame{AssetID: "unknown"}
	planned := PlanPhotoBestShots([]PhotoBestShotFrame{b, unknown, a})
	require.Len(t, planned, 2)
	require.Equal(t, "a", planned[0].AssetID)
	require.Equal(t, "pick", planned[0].Flag)
	require.Equal(t, "reject", planned[1].Flag)
	require.InDelta(t, (84.5-80.5)/84.5, planned[0].Confidence, 0.0001)
	require.Empty(t, PlanPhotoBestShots([]PhotoBestShotFrame{a, unknown}))
	require.Empty(t, PlanPhotoBestShots([]PhotoBestShotFrame{
		{AssetID: "a", Signals: map[string]float64{"focus": 0.8}},
		{AssetID: "b", Signals: map[string]float64{"framing": 0.9}},
	}))
	a.Signals = PhotoBestShotSignals(PhotoQualitySignals{
		Focus: 0.8, Framing: 0.1,
		FaceReports: []PhotoFaceReport{
			{Prominence: PhotoFaceProminenceFloor, EyesOpen: new(false)},
			{Prominence: 0.5, EyesOpen: new(true)},
		},
	})
	b.Signals = PhotoBestShotSignals(PhotoQualitySignals{
		Focus: 0.4, Framing: 0.9,
		FaceReports: []PhotoFaceReport{
			{Prominence: PhotoFaceProminenceFloor, EyesOpen: new(true)},
			{Prominence: 0.02, EyesOpen: new(false)},
			{Prominence: 0.5},
		},
	})
	require.Equal(t, map[string]float64{"focus": 0.8, "aesthetics": 0, "framing": 0.1, "eyes": 0}, a.Signals)
	require.Equal(t, map[string]float64{"focus": 0.4, "aesthetics": 0, "framing": 0.9, "eyes": 1}, b.Signals)
	eyes := PlanPhotoBestShots([]PhotoBestShotFrame{a, b})
	require.Len(t, eyes, 2)
	require.Equal(t, "b", eyes[0].AssetID)
	require.InDelta(t, (170.5-84.5)/170.5, eyes[0].Confidence, 0.0001)
	require.Equal(t, map[string]float64{"focus": 0, "aesthetics": 0, "framing": 0}, PhotoBestShotSignals(PhotoQualitySignals{
		FaceReports: []PhotoFaceReport{
			{Prominence: 0.02, EyesOpen: new(false)},
			{Prominence: 0.5},
		},
	}))
	b.Signals = a.Signals
	b.Raw = true
	ties := PlanPhotoBestShots([]PhotoBestShotFrame{a, b})
	require.Equal(t, "b", ties[0].AssetID)
	require.Equal(t, "Equal measurements; tie broken by RAW preference and photo ID", ties[1].Rationale)
	b.Raw = false
	require.Equal(t, "a", PlanPhotoBestShots([]PhotoBestShotFrame{b, a})[0].AssetID)
}

func TestPhotoFaceReportGrade(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report PhotoFaceReport
		grade  string
	}{
		{"unmeasured", PhotoFaceReport{}, ""},
		{"subject red", PhotoFaceReport{Light: new(.2), Prominence: .1}, "red"},
		{"yellow boundary", PhotoFaceReport{Light: new(.33), Prominence: .1}, "yellow"},
		{"green boundary", PhotoFaceReport{Light: new(.42), Prominence: .1}, "green"},
		{"small face", PhotoFaceReport{Light: new(.2), Prominence: .02}, "yellow"},
		{"subject boundary", PhotoFaceReport{Light: new(.2), Prominence: PhotoFaceProminenceFloor}, "red"},
		{"facing", PhotoFaceReport{Light: new(.9), Facing: new(.2), Prominence: .1}, "red"},
		{"eyes closed", PhotoFaceReport{Light: new(.9), EyesOpen: new(false), Prominence: .1}, "red"},
		{"eyes open", PhotoFaceReport{EyesOpen: new(true), Prominence: .1}, "green"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.grade, tc.report.ReportGrade())
		})
	}
}

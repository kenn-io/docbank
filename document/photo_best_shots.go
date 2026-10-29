package document

import (
	"cmp"
	"slices"
)

const PhotoFaceProminenceFloor = 0.021

// PhotoFaceReport keeps unknown observations absent. Smile is descriptive.
type PhotoFaceReport struct {
	Grade      string   `json:"grade,omitempty" enum:",red,yellow,green"`
	FaceID     string   `json:"face_id"`
	Sharpness  *float64 `json:"sharpness"`
	Light      *float64 `json:"light"`
	Facing     *float64 `json:"facing"`
	Prominence float64  `json:"prominence"`
	EyesOpen   *bool    `json:"eyes_open"`
	Smile      *bool    `json:"smile"`
}

func (r PhotoFaceReport) Worst() *float64 {
	var worst *float64
	for _, v := range []*float64{r.Sharpness, r.Light, r.Facing, photoBoolScore(r.EyesOpen)} {
		if v != nil && (worst == nil || *v < *worst) {
			worst = v
		}
	}
	return worst
}

func (r PhotoFaceReport) ReportGrade() string {
	worst := r.Worst()
	if worst == nil {
		return ""
	}
	if *worst < 0.33 && r.Prominence >= PhotoFaceProminenceFloor {
		return "red"
	}
	if *worst < 0.42 {
		return "yellow"
	}
	return "green"
}

func photoBoolScore(v *bool) *float64 {
	if v == nil {
		return nil
	}
	if *v {
		return new(1.0)
	}
	return new(0.0)
}

// PhotoBestShotFrame is one stack member's available measurements.
type PhotoBestShotFrame struct {
	AssetID string
	Raw     bool
	Signals map[string]float64
}

type PhotoBestShotProposal struct {
	AssetID    string  `json:"asset_id"`
	Flag       string  `json:"flag" enum:"pick,reject"`
	Rationale  string  `json:"rationale"`
	Confidence float64 `json:"confidence" minimum:"0" maximum:"1"`
}

func PhotoBestShotSignals(s PhotoQualitySignals) map[string]float64 {
	out := map[string]float64{"focus": s.Focus, "aesthetics": s.Aesthetics, "framing": s.Framing}
	for _, r := range s.FaceReports {
		if r.Prominence < PhotoFaceProminenceFloor {
			continue
		}
		if eyes := photoBoolScore(r.EyesOpen); eyes != nil {
			if v, ok := out["eyes"]; !ok || *eyes < v {
				out["eyes"] = *eyes
			}
		}
	}
	return out
}

// PlanPhotoBestShots ranks only kinds measured in every evaluated member.
// Unmeasured members never receive CUT. Confirmed choices are excluded before planning.
func PlanPhotoBestShots(frames []PhotoBestShotFrame) []PhotoBestShotProposal {
	weights := map[string]float64{"focus": 100, "eyes": 90, "aesthetics": 50, "framing": 45}
	measured := slices.DeleteFunc(slices.Clone(frames), func(f PhotoBestShotFrame) bool { return len(f.Signals) == 0 })
	if len(measured) < 2 {
		return nil
	}
	for kind := range weights {
		for _, f := range measured {
			_, ok := f.Signals[kind]
			if !ok {
				delete(weights, kind)
				break
			}
		}
	}
	if len(weights) == 0 {
		return nil
	}
	score := func(f PhotoBestShotFrame) float64 {
		var total float64
		// Fixed order keeps floating-point sums deterministic.
		for _, k := range []string{"focus", "eyes", "aesthetics", "framing"} {
			total += f.Signals[k] * weights[k]
		}
		return total
	}
	slices.SortFunc(measured, func(a, b PhotoBestShotFrame) int {
		if c := cmp.Compare(score(b), score(a)); c != 0 {
			return c
		}
		if a.Raw != b.Raw {
			if a.Raw {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.AssetID, b.AssetID)
	})
	confidence := 0.0
	if best := score(measured[0]); best > 0 {
		confidence = (best - score(measured[1])) / best
	}
	var out []PhotoBestShotProposal
	for i, f := range measured {
		flag, reason := "reject", "Weaker measured frame in this stack"
		if i == 0 {
			flag, reason = "pick", "Best measured frame in this stack"
		}
		out = append(out, PhotoBestShotProposal{f.AssetID, flag, reason, confidence})
	}
	return out
}

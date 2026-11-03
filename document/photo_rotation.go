package document

import "math"

// PhotoRotationFromRolls returns a clockwise correction when all measured faces
// agree within 25 degrees of the same nonzero quarter-turn. Rolls are clockwise
// radians in the oriented preview; missing measurements do not vote.
func PhotoRotationFromRolls(rolls []*float64) *int {
	var agreed *int
	for _, roll := range rolls {
		if roll == nil {
			continue
		}
		degrees := math.Mod(math.Mod(-*roll*180/math.Pi, 360)+360, 360)
		angle := -1
		for _, target := range []int{0, 90, 180, 270, 360} {
			if math.Abs(degrees-float64(target)) <= 25 {
				angle = target % 360
				break
			}
		}
		if angle <= 0 || agreed != nil && *agreed != angle {
			return nil
		}
		agreed = new(angle)
	}
	return agreed
}

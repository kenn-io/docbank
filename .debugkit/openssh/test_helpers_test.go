package openssh

func testTarget(value string) Target {
	target, err := ParseTarget(value)
	if err != nil {
		panic(err)
	}
	return target
}

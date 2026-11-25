package studioapi

// Review and publication request types (review.go), checked by
// TestRequestTypesAreSupported.
func init() {
	checkedRequestTypes = append(checkedRequestTypes,
		requestType{&decisionRequest{}, decisionKeys},
		requestType{&struct{}{}, nil}, // publish
	)
}

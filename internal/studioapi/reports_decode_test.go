package studioapi

// Error report request types (reports.go), checked by
// TestRequestTypesAreSupported.
func init() {
	checkedRequestTypes = append(checkedRequestTypes,
		requestType{&resolveRequest{}, resolveKeys},
	)
}

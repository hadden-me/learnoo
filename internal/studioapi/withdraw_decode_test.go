package studioapi

// Withdrawal, unpublication and restoration request types (withdraw.go),
// checked by TestRequestTypesAreSupported.
func init() {
	checkedRequestTypes = append(checkedRequestTypes,
		requestType{&withdrawRequest{}, withdrawKeys},
		requestType{&restoreRequest{}, restoreKeys},
	)
}

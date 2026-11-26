package studioapi

// Administration request types (admin.go), checked by
// TestRequestTypesAreSupported.
func init() {
	checkedRequestTypes = append(checkedRequestTypes,
		requestType{&grantRequest{}, grantKeys},
		requestType{&invitationRequest{}, invitationKeys},
		requestType{&namesRequest{}, namesKeys},
		requestType{&orderRequest{}, orderKeys},
	)
}

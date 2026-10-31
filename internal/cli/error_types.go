package cli

// Local error categories are separate from API error types, which pass through
// unchanged. Keep each locally emitted category in localErrorTypes as well.
const (
	errorTypeAPI                  = "api_error"
	errorTypeAuthentication       = "authentication_error"
	errorTypeAuthorization        = "authorization_error"
	errorTypeConfiguration        = "configuration_error"
	errorTypeConfirmationRequired = "confirmation_required"
	errorTypeInternal             = "internal_error"
	errorTypeNetwork              = "network_error"
	errorTypeRateLimit            = "rate_limit_error"
	errorTypeTimeout              = "timeout_error"
	errorTypeUpgrade              = "upgrade_error"
	errorTypeUsage                = "usage_error"
	errorTypeValidation           = "validation_error"
	errorTypeWaitConditionUnmet   = "wait_condition_unmet"
)

var localErrorTypes = []string{
	errorTypeAPI,
	errorTypeAuthentication,
	errorTypeAuthorization,
	errorTypeConfiguration,
	errorTypeConfirmationRequired,
	errorTypeInternal,
	errorTypeNetwork,
	errorTypeRateLimit,
	errorTypeTimeout,
	errorTypeUpgrade,
	errorTypeUsage,
	errorTypeValidation,
	errorTypeWaitConditionUnmet,
}

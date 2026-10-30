package cli

// Local error categories are separate from API error types, which pass through
// unchanged. Keep each locally emitted category in localErrorTypes as well.
const (
	errorTypeAPI                  = "api_error"
	errorTypeAuthorization        = "authorization_error"
	errorTypeConfiguration        = "configuration_error"
	errorTypeConfirmationRequired = "confirmation_required"
	errorTypeInternal             = "internal_error"
	errorTypeNetwork              = "network_error"
	errorTypeRateLimit            = "rate_limit_error"
	errorTypeTimeout              = "timeout_error"
	errorTypeUnavailable          = "unavailable_error"
	errorTypeUpgrade              = "upgrade_error"
	errorTypeUsage                = "usage_error"
	errorTypeWaitConditionUnmet   = "wait_condition_unmet"
)

var localErrorTypes = []string{
	errorTypeAPI,
	errorTypeAuthorization,
	errorTypeConfiguration,
	errorTypeConfirmationRequired,
	errorTypeInternal,
	errorTypeNetwork,
	errorTypeRateLimit,
	errorTypeTimeout,
	errorTypeUnavailable,
	errorTypeUpgrade,
	errorTypeUsage,
	errorTypeWaitConditionUnmet,
}

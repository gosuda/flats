package core

// Windows has no SIGHUP, and `flats install` restarts the service.
const (
	reloadAdvice      = "restart Flats with `flats install`"
	reloadServeAdvice = reloadAdvice
)

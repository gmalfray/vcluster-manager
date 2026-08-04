package models

// Actor represents the authenticated user performing an action, decoupled from
// the HTTP transport. The web/REST adapters build it from the request (via the
// auth package) and pass it to the service layer, so the service never depends
// on *http.Request for identity or RBAC.
type Actor struct {
	Username string
	IsAdmin  bool
}

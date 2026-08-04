package service

import (
	"errors"
	"regexp"
)

// nameRegex is the accepted shape of a vcluster name (also a K8s namespace
// suffix): starts with a letter, then lowercase alphanumerics and dashes. It
// already rules out "..", "/" and anything else a path-traversal attempt would
// need. Moved here from the handlers package during the service-layer
// extraction so every domain (and every adapter) validates names against a
// single copy instead of each keeping its own.
var nameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// validName reports whether name matches nameRegex.
func validName(name string) bool {
	return nameRegex.MatchString(name)
}

// ValidName is validName, exported for callers outside this package that need
// to check a vcluster name before it reaches something that isn't a service
// method — a handler validating a path parameter before handing it to a
// client-go call, for instance. Everything inside the service package should
// keep using the unexported validName.
func ValidName(name string) bool {
	return validName(name)
}

// ErrInvalidName means the requested name doesn't match nameRegex. Not every
// domain wires this in yet — it's here so the ones that do (and the ones
// extracted later) share the same sentinel instead of each declaring their own.
var ErrInvalidName = errors.New("invalid vcluster name")

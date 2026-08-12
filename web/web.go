// Package web carries the operator-facing static assets.
//
// They are compiled into the binary with go:embed rather than read from disk at
// boot. That is a real operational win over the Node original: the deployed
// artefact is ONE file with no sibling directory to lose, so a container that
// starts at all is guaranteed to have a working dashboard — there is no
// "works locally, 500s in prod because the HTML wasn't copied" failure mode.
package web

import _ "embed"

//go:embed dashboard.html
var DashboardHTML []byte

//go:embed admin.html
var AdminHTML []byte

//go:embed logo.svg
var LogoSVG []byte

// Package app is Papa's single-instance composition root. It creates services,
// registers public extensions, binds protected HTTP routes and coordinates
// startup, cancellation and draining before shared resources are closed.
// Domain state and transactions belong to the owning services; feature HTTP,
// page resources and presentation belong to admin packages. Registration is
// completed before Run. Public aliases expose the current v4 declarations and extension APIs.
package app

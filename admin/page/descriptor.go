// Package page defines feature-owned page metadata and embedded resources.
// It is a value contract shared by features and application composition, with
// no dependency on the shell, server or application.
package page

// Descriptor supplies a feature's default page. The host decides whether and
// where to mount it; Script and CSS retain their existing resource order.
type Descriptor struct {
	Key, Label, Group, Section string
	Script, CSS                string
}

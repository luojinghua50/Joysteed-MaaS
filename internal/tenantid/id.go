// Package tenantid defines the dependency-free tenant identity type shared by
// control-plane packages. The data-plane tenant package aliases this type so
// existing APIs remain source-compatible without pulling Bifrost into the
// standalone control-plane binary.
package tenantid

type ID string

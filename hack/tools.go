//go:build tools
// +build tools

//
// This is used to ensure that controller-gen is included in the /vendor directory.  See
// https://stackoverflow.com/questions/52428230/how-do-go-modules-work-with-installable-commands.
package hack

import (
	_ "github.com/arttor/helmify/cmd/helmify"
	_ "honnef.co/go/tools/cmd/staticcheck"
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)

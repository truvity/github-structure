// Package staterename re-keys a Pulumi stack's state when a repository row
// is renamed.
//
// A resource's name is part of its URN, and every resource of a repository
// row carries the repository's name: the repository, its Actions settings,
// its branch protection, its rulesets and its team grants. Renaming the row
// alone therefore plans a create of every resource under the new name and
// a delete of every one under the old. The repository on GitHub is the same
// object, so the safe route is a state operation: rename the repository on
// GitHub, rename the row, re-key the state, and a refresh plans nothing.
// docs/safety.md describes the route; this package is its one
// implementation.
//
// Rekey is a pure transform of the exported state. It does no I/O and knows
// no backend, so a consumer wires it to whichever way it exports and
// imports a stack, and the same function is run by the CI-enforced tests
// and by the operator.
package staterename

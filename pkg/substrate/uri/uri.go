// Package uri re-exports the public surface of pkg/uri as
// pkg/substrate/uri, per ADR-100 Step 2b.
//
// All symbols here are Go type aliases or variable/function aliases of their
// counterparts in github.com/myrgic/cogos/pkg/uri. Consumers of either
// import path get identical types at the language level — no conversion needed.
//
// This package adds no logic. It is a pure re-export layer so downstream code
// can migrate incrementally to the substrate import path without changing
// call sites or type assertions.
package uri

import uri "github.com/myrgic/cogos/pkg/uri"

// --- Scheme constants ---

const Scheme       = uri.Scheme
const SchemeLegacy = uri.SchemeLegacy

// --- Sentinel errors ---

var ErrInvalidURI      = uri.ErrInvalidURI
var ErrUnknownNamespace = uri.ErrUnknownNamespace

// --- Types ---

type URI   = uri.URI
type Error = uri.Error

// --- Namespace registry ---

var Namespaces = uri.Namespaces

// --- Functions ---

var Parse              = uri.Parse
var IsCogURI           = uri.IsCogURI
var IsValidNamespace   = uri.IsValidNamespace
var ExtractInlineRefs  = uri.ExtractInlineRefs

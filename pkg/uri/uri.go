// Package uri is the legacy import path for what is now the canonical
// package at github.com/myrgic/cogos/pkg/substrate/uri.
//
// Per ADR-100, the substrate layer is the architectural source of truth.
// As of the canonical-vs-shim inversion (2026-05-23), the source-of-truth
// implementation lives at pkg/substrate/uri/. This package retains the legacy
// import path as a thin re-export shim so external consumers that still
// import the legacy path continue to compile without source changes.
//
// All symbols here are Go type aliases or variable aliases of their
// counterparts in github.com/myrgic/cogos/pkg/substrate/uri. Consumers of
// either import path get identical types at the language level.
//
// This shim adds no logic. New code should import the substrate path directly.
package uri

import uri "github.com/myrgic/cogos/pkg/substrate/uri"

// --- Scheme constants ---

const Scheme = uri.Scheme
const SchemeLegacy = uri.SchemeLegacy

// --- Sentinel errors ---

var ErrInvalidURI = uri.ErrInvalidURI
var ErrUnknownNamespace = uri.ErrUnknownNamespace

// --- Types ---

type URI = uri.URI
type Error = uri.Error

// --- Namespace registry ---

var Namespaces = uri.Namespaces

// --- Functions ---

var Parse = uri.Parse
var IsCogURI = uri.IsCogURI
var IsValidNamespace = uri.IsValidNamespace
var ExtractInlineRefs = uri.ExtractInlineRefs

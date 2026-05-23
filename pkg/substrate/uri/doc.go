// Package uri provides parsing, formatting, and validation of cog: URIs.
//
// A cog: URI addresses a workspace resource (memory document, agent definition,
// configuration file, etc.) without coupling to its filesystem location.
//
// URI format (ADR-067):
//
//	cog:namespace/path[?query][#fragment]          (local — bare canonical form)
//	cog://workspace/namespace/path[?query][#fragment]  (cross-workspace authority form)
//
// The bare form is canonical for local references; the authority form is used
// for cross-workspace references where the workspace name is the authority.
//
// Examples:
//
//	cog:mem/semantic/insights/eigenform.cog.md
//	cog:mem/semantic/insights/eigenform.cog.md#Seed
//	cog:conf/kernel.yaml
//	cog:crystal
//	cog:signals/inference?above=0.3
//	cog://other-workspace/mem/semantic/foo.cog.md  (cross-workspace)
//
// This package answers "what is a cog: URI and how do I parse/format one?"
// Resolution — looking up what a URI points to on disk — stays in the kernel.
package uri

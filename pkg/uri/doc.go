// Package uri provides parsing, formatting, and validation of cog:// URIs.
//
// A cog:// URI addresses a workspace resource (memory document, agent definition,
// configuration file, etc.) without coupling to its filesystem location.
//
// URI format:
//
//	cog://namespace/path[?query][#fragment]
//
// Examples:
//
//	cog://mem/semantic/insights/eigenform.cog.md
//	cog://mem/semantic/insights/eigenform.cog.md#Seed
//	cog://conf/kernel.yaml
//	cog://crystal
//	cog://signals/inference?above=0.3
//
// This package answers "what is a cog:// URI and how do I parse/format one?"
// Resolution — looking up what a URI points to on disk — stays in the kernel.
package uri

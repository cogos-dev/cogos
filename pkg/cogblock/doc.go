// Package cogblock implements the content-addressed block format for CogOS.
//
// A CogBlock is the fundamental unit of structured data exchange: a typed,
// content-addressed envelope that carries arbitrary payloads with integrity
// guarantees. Blocks are identified by the hash of their canonical encoding,
// making them suitable for deduplication, caching, and verified transport.
package cogblock

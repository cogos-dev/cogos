// Package channel is the substrate-canonical home for channel schema —
// the data types describing channel-to-Discord bridge mappings, plus the
// YAML loader. It contains no routing logic, no daemon dependencies, and
// no kernel-only types.
//
// Per ADR-100 Step 3, this package was extracted from the root
// channel_config.go file. The root file is now a thin re-export shim
// that aliases these types so existing callers compile unchanged.
//
// The routing implementation (Discord inlet, agent dispatch, kernel
// reconciliation) remains in the root package main. Per RFC-034's
// substrate-vs-kernel cut: schema + loaders here; daemon-required
// behavior in the kernel.
package channel

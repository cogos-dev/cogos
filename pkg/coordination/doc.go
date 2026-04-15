// Package coordination provides distributed claim, release, and checkpoint
// primitives for multi-node CogOS clusters.
//
// It implements lease-based ownership of resources and durable checkpointing
// so that work can be resumed after node restarts or handed off between nodes.
package coordination

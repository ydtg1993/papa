// Package taskstore owns durable task transitions and their SQL fences.
// It depends on task models and compensation contracts, never on Engine,
// worker scheduling or HTTP. Callers retain retries and lifecycle orchestration.
package taskstore

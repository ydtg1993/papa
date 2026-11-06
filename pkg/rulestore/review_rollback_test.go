package rulestore

import (
	"context"
	"errors"
	"testing"

	"github.com/ydtg1993/papa/v3/models"
)

func TestMySQLRollbackRejectsCorruptHistoricalProofWithoutMovingCurrent(t *testing.T) {
	r, db, ref, prefix, proof := reviewVerifiedRule(t)
	ctx := context.Background()
	actor := Actor{"review", "rollback proof integrity"}
	bindings := map[string]Reference{"catalog": ref}
	first, err := r.Publish(ctx, prefix+"-first", prefix, 0, bindings, actor)
	if err != nil {
		t.Fatal(err)
	}
	newProof, err := r.Verify(ctx, ref, proof.SetID, actor)
	if err != nil || newProof.ID == proof.ID {
		t.Fatalf("new verification: %+v %v", newProof, err)
	}
	current, err := r.Publish(ctx, prefix+"-second", prefix, first.Generation, bindings, actor)
	if err != nil {
		t.Fatal(err)
	}
	proof.Report.Results = proof.Report.Results[:1]
	data, err := encode(proof.Report, MaxMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RuleVerification{}).Where("id = ?", proof.ID).Update("report_json", string(data)).Error; err != nil {
		t.Fatal(err)
	}
	opID := prefix + "-rollback"
	if result, err := r.Rollback(ctx, opID, prefix, current.Generation, actor); !errors.Is(err, ErrCorrupt) || result.ID != "" {
		t.Fatalf("corrupt historical proof accepted: result=%+v err=%v", result, err)
	}
	got, err := r.Current(ctx, prefix)
	if err != nil || got.ID != current.ID || got.Generation != current.Generation || got.VerificationIDs["catalog"] != newProof.ID {
		t.Fatalf("failed rollback changed current: %+v %v", got, err)
	}
	if _, err := r.LookupOperation(ctx, opID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed rollback left durable operation: %v", err)
	}
	history, err := r.History(ctx, prefix, 10)
	if err != nil || len(history) != 2 {
		t.Fatalf("failed rollback changed history: %+v %v", history, err)
	}
}

// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package runtimev1

import "testing"

// The wire surface of omnia.runtime.v1 that ContractVersion describes, as
// Omnia pins it for the same version. Equal fingerprints mean these stubs and
// Omnia's facade agree on every message, field, enum value and RPC. When the
// proto changes, the fingerprint moves: bump ContractVersion and the proto
// marker together, and pin the new pair here.
const (
	surfaceVersion     = "1.15.0"
	surfaceFingerprint = "7f6c067bda00ce45"
)

func TestSurfaceMatchesTheContractVersion(t *testing.T) {
	if got := Fingerprint(File_omnia_runtime_v1_runtime_proto); got != surfaceFingerprint {
		t.Fatalf("omnia.runtime.v1 surface changed (fingerprint %s, pinned %s for %s): bump ContractVersion "+
			"and the proto's Contract-Version marker, then pin the new pair", got, surfaceFingerprint, surfaceVersion)
	}
	if ContractVersion != surfaceVersion {
		t.Fatalf("ContractVersion = %q but the surface is pinned for %q", ContractVersion, surfaceVersion)
	}
}

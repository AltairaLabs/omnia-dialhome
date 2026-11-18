// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package runtimev1

import (
	"os"
	"regexp"
	"testing"
)

func TestContractVersionMatchesTheProto(t *testing.T) {
	raw, err := os.ReadFile("../proto/omnia/runtime/v1/runtime.proto")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^// Contract-Version: (\S+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("runtime.proto has no Contract-Version marker")
	}
	if got := string(m[1]); got != ContractVersion {
		t.Fatalf("proto says %s, ContractVersion says %s", got, ContractVersion)
	}
}

// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package runtimev1

// ContractVersion is the omnia.runtime.v1 contract version these stubs were
// generated from. It mirrors the `// Contract-Version:` marker in
// proto/omnia/runtime/v1/runtime.proto, and contract_test.go asserts the two
// agree. A runtime reports it from Health as contract_version.
const ContractVersion = "1.15.0"

// PackDigestHeader is the gRPC response header on which a runtime announces
// the digest ("sha256:<hex>") of the PromptPack a Converse stream or Invoke
// call actually ran. It is sent only when the runtime knows a digest.
const PackDigestHeader = "x-omnia-pack-digest"

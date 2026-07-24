# Vendored data-labour-prov ontology (conformance fixtures)

These two files are **verbatim copies** vendored for the exporter's SHACL/riot
conformance tests. They are the contract the `provo` exporter's Turtle output must
conform to (`provo` is the vocabulary's reference implementation).

| file | purpose |
|---|---|
| `data-labour-prov.ttl` | the OWL vocabulary (`:LLMAgent`, `:modelId`, `:derivationKind`, …) |
| `shapes.ttl` | the SHACL shapes the exporter output is validated against |

**Source:** `ontology/{data-labour-prov,shapes}.ttl` in the `taxonomy-of-benchmarks`
originating repository.

**Vendored from commit** `e8ca4b9f65dac7495b2ffbea3d2b8d9aac4741ba` (2026-07-18),
copied **2026-07-23**.

To refresh: re-copy both files from the originating repo's `ontology/` directory and update
the commit hash + date above. The namespace `https://example.org/data-labour-prov#` is
a placeholder pending w3id registration (do not treat as permanent).

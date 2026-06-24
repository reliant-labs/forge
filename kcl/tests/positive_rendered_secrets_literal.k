"""
Env-gated positive test: RenderedSecrets from='literal' is allowed in
dev/e2e. Run with `-D env=dev` and `-D env=e2e` (see
TestKCLModule_RenderedSecretsLiteral). A literal inline value is fine for
non-sensitive fixed test values where a dotenv round-trip is friction.

This file is EXCLUDED from the default positive runner (it requires the
`-D env=` binding the gate checks); a render with no env binding correctly
REJECTS the literal (see negative_rendered_secrets_literal_prod.k).
"""

import forge

_bundle = forge.Bundle {
    services = []
    secret_provider = forge.RenderedSecrets {
        secrets = [
            forge.RenderedSecret {
                name = "test-fixtures"
                keys = {
                    "ISSUER" = forge.RenderedSecretKey {
                        from = "literal"
                        value = "https://test.local/"
                    }
                }
            }
        ]
    }
}

output = forge.render(_bundle)

# The literal value projects (dev/e2e only — gated by the schema check
# which passed because `-D env` is dev/e2e).
assert_literal_value = output.secret_provider.secrets[0].keys.ISSUER.value == "https://test.local/"
assert_literal_from = output.secret_provider.secrets[0].keys.ISSUER.from == "literal"

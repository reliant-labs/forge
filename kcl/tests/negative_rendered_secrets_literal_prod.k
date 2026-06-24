"""
Negative test — RenderedSecrets from='literal' is rejected outside
dev/e2e. The default negative runner renders with NO `-D env` binding, so
option("env") is None — not in [dev, e2e] — and the per-key check fires.
(A prod render with `-D env=prod` is rejected the same way.)
"""

import forge

_bundle = forge.Bundle {
    services = []
    secret_provider = forge.RenderedSecrets {
        secrets = [
            forge.RenderedSecret {
                name = "leak"
                keys = {
                    "API_KEY" = forge.RenderedSecretKey {
                        from = "literal"
                        value = "sk-should-not-be-inlined-in-prod"
                    }
                }
            }
        ]
    }
}

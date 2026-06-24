"""
Negative test — TestJWKS.alg must be 'ES256' (the only supported
test-JWKS algorithm today). Any other value should fail the check block.
"""

import forge

_j = forge.TestJWKS {
    name = "test-jwks"
    kid = "k1"
    alg = "RS256"  # unsupported
    private_key = forge.DotenvRef { key = "TEST_JWKS_PEM" }
}

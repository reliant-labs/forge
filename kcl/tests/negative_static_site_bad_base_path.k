"""
Negative test — StaticSite.base_path must start with '/'.
A bare segment ("admin") fails the check block, exactly as it does for
FirebaseHosting (see negative_firebase_bad_base_path.k). The two static
targets must agree on base_path syntax: they feed the SAME assembler.
"""

import forge

_fe = forge.Frontend {
    name = "admin-web"
    path = "admin-web"
    deploy = forge.StaticSite {
        bucket = "gs://b"
        public_dir = "out"
        base_path = "admin"
    }
}

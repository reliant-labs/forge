"""
Negative test — FirebaseHosting.base_path must start with '/'.
A bare segment ("admin") fails the check block.
"""

import forge

_fe = forge.Frontend {
    name = "admin-web"
    path = "admin-web"
    deploy = forge.FirebaseHosting {
        project = "p"
        site = "s"
        public_dir = "out"
        base_path = "admin"
    }
}

"""
Negative test — a content route is a route pattern, matched against
page_viewed's route, which starts with "/" and never carries a query string.
"""

# expect: Analytics.content_routes has an entry that is not a route pattern

import forge

output = forge.render(forge.Bundle {
    project = "demo"
    frontends = [forge.Frontend {
        name = "web", path = "web", type = "vite", port = 5173
        runtime = forge.OnHost {}
        telemetry = forge.BrowserTelemetry {analytics = forge.Analytics {
            content_routes = ["/blog/*", "docs/*"]
        }}
    }]
})

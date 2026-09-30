package main

// logo is the mark the management clients show for this provider.
//
// It is defined HERE rather than added to `internal/jethub/brandicons` for a
// deliberate reason: that package is shared with the five sibling provider
// plugins, and adding a member to it would be an edit outside this plugin's
// directory. The shape matches the fallback the repository already uses where a
// vendor ships no usable artwork (a rounded badge with the provider's initial):
// an inline SVG data URL, so the panel needs no outbound request and renders the
// same in either theme.
const logo = `data:image/svg+xml,%3Csvg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"%3E%3Crect width="24" height="24" rx="6" fill="%231f6feb"/%3E%3Ctext x="12" y="17.5" font-size="15" font-family="sans-serif" font-weight="700" fill="white" text-anchor="middle"%3EZ%3C/text%3E%3C/svg%3E`

import Foundation

/// Looks `key` up in this module's string catalog (Resources/Localizable.xcstrings). Interpolated
/// values become format arguments: `localized("\(count) screenshots")` uses "%lld screenshots".
/// `Scripts/check-strings.py` keeps the catalog in step with these calls.
func localized(_ key: String.LocalizationValue) -> String {
    String(localized: key, bundle: .module)
}

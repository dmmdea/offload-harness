### Fixed - the gate page says what a judge receipt does and does not bind

- **The receipt is checked for its shape, not for what it hashes.** `gate verdict` accepts a model judge's receipt when
  its scorer id is the judge identity, `passed` is true and both hashes are 64 lower-case hex characters. It does not
  compare `pairs_hash` with a pairs file or `build_sha256` with any build, so a receipt written by an older build still
  certifies a scorer that kept its id. The page said the receipt "binds" the build; it now says the receipt records it,
  states what the gate checks, and says that a scorer whose behaviour changes gets a new receipt or a new id by
  convention, which the gate does not enforce. Docs only.

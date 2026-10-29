// Package skills owns on-demand procedural knowledge: directories holding a
// SKILL.md (YAML frontmatter plus a markdown body) that the model discovers
// through a compact name-and-description index and loads in full only when
// chosen. Only selection metadata rides the system prompt; bodies load
// through the model-facing tool or resolve fresh on each use, so a large
// library never bloats every turn.
//
// Two roots feed one catalog in fixed precedence: the profile-managed
// <profileRoot>/skills/ tree (writable through the tool) beats the
// operator-configured external library directory (read-only). Same-named
// copies resolve by tier, then by exact relative path; anything malformed
// is skipped, never fatal.
package skills

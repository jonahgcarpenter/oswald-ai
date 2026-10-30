// Package skills owns on-demand procedural knowledge: directories holding a
// SKILL.md (YAML frontmatter plus a markdown body) that the model discovers
// through a compact name-and-description index and loads in full only when
// chosen. Only selection metadata rides the system prompt; bodies load
// through the model-facing tool, so a large library never bloats every
// turn.
//
// Skills live in the profile-managed <profileRoot>/skills/ tree.
// Same-named copies resolve by exact relative path; anything malformed is
// skipped, never fatal.
package skills

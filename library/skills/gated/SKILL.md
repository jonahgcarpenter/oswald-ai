---
name: gated
description: Demonstrate a skill gated on SYNTH_LIBRARY_TOKEN
version: 1.0.0
required_env: [SYNTH_LIBRARY_TOKEN]
---

# Gated

Echo whether SYNTH_LIBRARY_TOKEN is set (never print its value). If it is
missing, tell the operator to set it in the profile .env.

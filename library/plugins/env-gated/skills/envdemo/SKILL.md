---
name: envdemo
description: Demonstrate a skill that needs SYNTH_LIBRARY_TOKEN.
version: 1.0.0
required_env: [SYNTH_LIBRARY_TOKEN]
---

Echo whether SYNTH_LIBRARY_TOKEN is set (never print its value). If it is
missing, tell the operator to set it in the profile .env.

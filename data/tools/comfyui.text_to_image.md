# comfyui.text_to_image

## Description

Generate one image from a concise, concrete visual description using the operator's configured SD3.5 Turbo checkpoint. Put the subject and required attributes first, including visible features that distinguish it from similar objects. Describe composition, lighting, and style when relevant to the user's request; do not add unrequested embellishments. Avoid conversational instructions, contradictory styles, and piles of generic quality keywords. Use a short, targeted negative_prompt only for unwanted visual elements, not a universal negative list. The returned image is attached to the response.

Keep the operator defaults unless the request calls for a change. `steps`, `cfg`, `sampler_name`, `scheduler`, and `shift` tune only the sampling process: they do not change the loaded model or the image size, and they are safe to adjust. Omission preserves the operator template. The operator owns the model, the CLIP/VAE weight files, and the generation dimensions.

Use this tool to create new imagery, not to find or show an existing real-world image. Use `web.image_search` for visual research and `web.image_select` to deliver a found preview. When visual references would inform generation, inspect them before composing the generation prompt. Precise descriptions help conditioning but do not guarantee object geometry or readable text.

Each successful call creates a new logical image with server-assigned image_id and version 1. Refine it using image_to_image rather than creating repeated drafts with this tool. Only the latest successful version per logical image produced this request is attached at final delivery. At most four logical images can be delivered per request; intermediate versions remain temporary editing sources. Result source_image_id is the exact immutable asset selector, distinct from image_id.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| prompt | string | yes | Concise visual description: subject and required distinguishing attributes first, then relevant composition, lighting, and style |
| negative_prompt | string | no | Short, targeted list of unwanted visual elements; avoid generic negative lists |
| steps | integer | no | Sampling steps from 1 to 50; omitted uses the operator template |
| cfg | number | no | Classifier-free guidance scale from 0 to 10; omitted uses the operator template |
| sampler_name | string | no | Supported KSampler sampler; omitted uses the operator template |
| scheduler | string | no | Supported KSampler scheduler; omitted uses the operator template |
| shift | number | no | SD3 model-sampling shift from 0.1 to 10; omitted uses the operator template |
| seed | integer | no | Fixed seed from 0 to 4294967295 for reproducibility; omitted generates a fresh random seed |

## Schema

```json
{
  "type": "object",
  "properties": {
    "prompt": {"type": "string", "description": "Concise visual description with subject and required distinguishing attributes first, then relevant composition, lighting, and style. Avoid conflicting styles, generic quality keywords, and unrequested embellishments", "minLength": 1, "maxLength": 2000},
    "negative_prompt": {"type": "string", "description": "Short, targeted list of unwanted visual elements; avoid generic negative lists", "maxLength": 2000},
    "steps": {"type": "integer", "description": "Sampling steps from 1 to 50; omitted preserves the operator template", "minimum": 1, "maximum": 50},
    "cfg": {"type": "number", "description": "Classifier-free guidance scale from 0 to 10; omitted preserves the operator template", "minimum": 0, "maximum": 10},
    "sampler_name": {"type": "string", "description": "Sampler used by the KSampler node; omitted preserves the operator template", "enum": ["euler", "euler_ancestral", "heun", "dpm_2", "dpm_2_ancestral", "lms", "dpmpp_2s_ancestral", "dpmpp_2m", "dpmpp_2m_sde", "dpmpp_sde", "dpmpp_3m_sde", "ddim", "uni_pc", "uni_pc_bh2"]},
    "scheduler": {"type": "string", "description": "Noise scheduler used by the KSampler node; omitted preserves the operator template", "enum": ["normal", "karras", "exponential", "sgm_uniform", "simple", "ddim_uniform", "beta", "linear_quadratic", "kl_optimal"]},
    "shift": {"type": "number", "description": "SD3 model-sampling shift from 0.1 to 10; omitted preserves the operator template", "minimum": 0.1, "maximum": 10},
    "seed": {"type": "integer", "description": "Fixed seed from 0 to 4294967295 for a reproducible result; omitted generates a fresh random seed", "minimum": 0, "maximum": 4294967295}
  },
  "required": ["prompt"],
  "additionalProperties": false
}
```

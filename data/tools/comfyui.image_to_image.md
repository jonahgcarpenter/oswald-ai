# comfyui.image_to_image

## Description

Transform an available image according to a detailed visual prompt using the operator's configured SD3.5 Turbo checkpoint. Describe the desired FINAL image, emphasizing the changed attribute and what should remain, rather than a bare edit command. The operator resizes the selected source to a fixed, safe resolution before encoding, so edits cannot increase VRAM beyond the operator configuration. Select source_image_id from the session image catalog. If omitted, use the latest output generated in this request, otherwise the first current attached/replied image, otherwise the latest retained delivered output. If no source is available, ask for an image or generate one first. Optional strength controls denoise from 0.1 to 0.9; omission preserves the operator template. Consider around 0.55-0.65 for visible changes, without guarantees; higher values can alter composition and identity. The returned image is attached to the response. Use negative_prompt only to describe visual elements that should be excluded.

Keep the operator defaults unless the request calls for a change. `steps`, `cfg`, `sampler_name`, `scheduler`, and `shift` tune only the sampling process: they do not change the loaded model, and they are safe to adjust. Omission preserves the operator template. The operator owns the model, the CLIP/VAE weight files, the resize dimensions, and the batch size.

Use this tool to modify imagery, not to retrieve or deliver an unchanged existing image. Use `web.image_search` for visual research and `web.image_select` to deliver a found preview. Search previews can inform the edit prompt after inspection but are not session-catalog editing sources. Generated output is not a substitute for finding or showing an existing real-world image.

By default, an edit creates the next server-assigned version of the selected logical image, replacing its earlier output in this response. Editing an older source still advances the version; parent_source_image_id identifies that exact source. Set create_variant=true to keep both alternatives as separate logical images. Only the latest successful version of each logical image produced this request is attached at final delivery (maximum four logical images). Failed edits keep the last success. Results include image_id, version, source_image_id, and parent_source_image_id; use only catalog source_image_id values for editing.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| prompt | string | yes | Desired final image, emphasizing the changed attribute and details to retain, not a bare edit command |
| negative_prompt | string | no | Visual elements and qualities to exclude |
| source_image_id | string | no | Available image ID from the session image catalog |
| create_variant | boolean | no | Create a separate logical image rather than replacing the selected image's output; defaults false |
| strength | number | no | Denoise from 0.1 to 0.9; omitted uses template. Consider 0.55-0.65 for visible changes; higher values can alter composition and identity |
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
    "create_variant": {"type": "boolean", "description": "Create a separate logical image to deliver both alternatives; default false advances the selected logical image's version"},
    "source_image_id": {"type": "string", "description": "Available image ID from the session image catalog"},
    "strength": {"type": "number", "description": "Denoise from 0.1 to 0.9; omission preserves operator template. Consider 0.55-0.65 for visible changes, without guarantees; higher values can alter composition and identity", "minimum": 0.1, "maximum": 0.9},
    "prompt": {"type": "string", "description": "Describe the desired FINAL image, emphasizing the changed attribute and details to retain, not a bare edit command", "minLength": 1, "maxLength": 2000},
    "negative_prompt": {"type": "string", "description": "Visual elements and qualities to exclude", "maxLength": 2000},
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

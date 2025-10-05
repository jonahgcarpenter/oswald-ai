from unsloth import FastLanguageModel  # isort: skip
import json
import sys

import torch
from datasets import Dataset
from interactive_test import interactive_test
from transformers import TrainingArguments
from trl import SFTTrainer

# Import Dataset
file = json.load(open("data.json", "r"))
print(file[1])

# Load the base model
model_name = "unsloth/Phi-3-mini-4k-instruct-bnb-4bit"

max_seq_length = 2048  # Choose sequence length
dtype = None  # Auto detection

# Load model and tokenizer
model, tokenizer = FastLanguageModel.from_pretrained(
    model_name=model_name,
    max_seq_length=max_seq_length,
    dtype=dtype,
    load_in_4bit=True,
)


# TODO: Remeber to edit per dataset
# Formatting the prompt for training
def format_prompt(example):
    return f"### Input: {example['text']}\n### Output: {json.dumps(example['search_queries'])}<|endoftext|>"


formatted_data = [format_prompt(item) for item in file]
dataset = Dataset.from_dict({"text": formatted_data})
print(dataset[0]["text"])

# Add LoRA adapters
model = FastLanguageModel.get_peft_model(
    model,
    r=64,  # LoRA rank - higher = more capacity, more memory
    target_modules=[
        "q_proj",
        "k_proj",
        "v_proj",
        "o_proj",
        "gate_proj",
        "up_proj",
        "down_proj",
    ],
    lora_alpha=128,  # LoRA scaling factor (usually 2x rank)
    lora_dropout=0,  # Supports any, but = 0 is optimized
    bias="none",  # Supports any, but = "none" is optimized
    use_gradient_checkpointing="unsloth",  # Unsloth's optimized version
    random_state=3407,
    use_rslora=False,  # Rank stabilized LoRA
    loftq_config=None,  # LoftQ
)

# Training arguments optimized for Unsloth
trainer = SFTTrainer(
    model=model,
    tokenizer=tokenizer,
    train_dataset=dataset,
    dataset_text_field="text",
    max_seq_length=max_seq_length,
    dataset_num_proc=2,
    args=TrainingArguments(
        per_device_train_batch_size=2,
        gradient_accumulation_steps=4,  # Effective batch size = 8
        warmup_steps=10,
        num_train_epochs=3,
        learning_rate=2e-4,
        fp16=not torch.cuda.is_bf16_supported(),
        bf16=torch.cuda.is_bf16_supported(),
        logging_steps=25,
        optim="adamw_8bit",
        weight_decay=0.01,
        lr_scheduler_type="linear",
        seed=3407,
        output_dir="outputs",
        save_strategy="epoch",
        save_total_limit=2,
        dataloader_pin_memory=False,
        report_to="none",  # Disable Weights & Biases logging
    ),
)

# Train the model
trainer_stats = trainer.train()

merged_model_path = "merged_16bit_model"
model.save_pretrained_merged(merged_model_path, tokenizer, save_method="merged_16bit")

model, tokenizer = FastLanguageModel.from_pretrained(
    model_name=merged_model_path,  # Load our saved model
    dtype=dtype,
    load_in_4bit=False,  # No need for 4-bit loading here
)

should_save = interactive_test(model, tokenizer)

if should_save:
    model.save_pretrained_gguf(
        "gguf_model", tokenizer, quantization_method="q4_k_m", maximum_memory_usage=0.4
    )
    print("\nGGUF model saved successfully.")
else:
    sys.exit()

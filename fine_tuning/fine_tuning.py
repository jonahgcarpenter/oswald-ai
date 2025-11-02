from unsloth import FastLanguageModel  # isort: skip
import json
import sys

import torch
from datasets import Dataset
from interactive_test import interactive_test
from transformers import TrainingArguments
from trl import SFTTrainer

file = json.load(open("data.json", "r"))
print(file[1])

model_name = "unsloth/Phi-3-mini-4k-instruct-bnb-4bit"

max_seq_length = 2048
dtype = None

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

model = FastLanguageModel.get_peft_model(
    model,
    r=64,
    target_modules=[
        "q_proj",
        "k_proj",
        "v_proj",
        "o_proj",
        "gate_proj",
        "up_proj",
        "down_proj",
    ],
    lora_alpha=128,
    lora_dropout=0,
    bias="none",
    use_gradient_checkpointing="unsloth",
    random_state=3407,
    use_rslora=False,
    loftq_config=None,
)

trainer = SFTTrainer(
    model=model,
    tokenizer=tokenizer,
    train_dataset=dataset,
    dataset_text_field="text",
    max_seq_length=max_seq_length,
    dataset_num_proc=2,
    args=TrainingArguments(
        per_device_train_batch_size=2,
        gradient_accumulation_steps=4,
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
        report_to="none",
    ),
)

trainer_stats = trainer.train()

merged_model_path = "merged_16bit_model"
model.save_pretrained_merged(merged_model_path, tokenizer, save_method="merged_16bit")

model, tokenizer = FastLanguageModel.from_pretrained(
    model_name=merged_model_path,
    dtype=dtype,
    load_in_4bit=False,
)

should_save = interactive_test(model, tokenizer)

if should_save:
    model.save_pretrained_gguf(
        "gguf_model", tokenizer, quantization_method="q4_k_m", maximum_memory_usage=0.4
    )
    print("\nGGUF model saved successfully.")
else:
    sys.exit()

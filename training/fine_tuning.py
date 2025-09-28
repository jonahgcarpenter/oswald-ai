from unsloth import FastLanguageModel  # isort: skip
import argparse
import json
import os

import torch
from datasets import Dataset
from sklearn.model_selection import train_test_split  # Import for splitting data
from transformers import TrainingArguments
from trl import SFTTrainer


def to_text(example, tokenizer):
    """Formats a data example into the required chat template format."""
    resp = example["label"]
    if not isinstance(resp, str):
        resp = json.dumps(resp, ensure_ascii=False)
    msgs = [
        {"role": "user", "content": example["text"]},
        {"role": "assistant", "content": resp},
    ]
    # The chat template formats the roles and adds the necessary EOS tokens
    return tokenizer.apply_chat_template(
        msgs, tokenize=False, add_generation_prompt=False
    )


def interactive_test(model, tokenizer, device):
    """Starts an interactive loop to test the model."""
    print("\n\n==============================================")
    print("  Entering Interactive Testing Mode")
    print("==============================================")
    print("Type your prompts and press Enter. Type 'exit' to save the model and quit.")

    # Enable inference mode for faster generation
    FastLanguageModel.for_inference(model)

    while True:
        user_prompt = input("\n>>> Prompt: ")
        if user_prompt.lower() == "exit":
            print("Exiting interactive mode...")
            break

        messages = [{"role": "user", "content": user_prompt}]
        inputs = tokenizer.apply_chat_template(
            messages,
            tokenize=True,
            add_generation_prompt=True,
            return_tensors="pt",
        ).to(device)

        # Generate the response
        outputs = model.generate(
            input_ids=inputs,
            max_new_tokens=256,
            use_cache=True,
            temperature=0.7,
            do_sample=True,
            top_p=0.9,
        )

        # Decode and print the response
        response = tokenizer.batch_decode(outputs)[0]
        print(f"MODEL RESPONSE:\n{response}")


def main(args):
    """Main function to run the fine-tuning pipeline."""
    # Check for GPU
    if torch.cuda.is_available():
        device = "cuda"
        print(f"GPU found: {torch.cuda.get_device_name(0)}")
    else:
        device = "cpu"
        print("No GPU found, running on CPU. This will be very slow.")

    # Load the training data
    try:
        with open(args.data_file, "r", encoding="utf-8") as f:
            data = json.load(f)
        print(f"Successfully loaded {len(data)} records from {args.data_file}")
    except FileNotFoundError:
        print(f"Error: Data file not found at {args.data_file}")
        return
    except json.JSONDecodeError:
        print(f"Error: Could not decode JSON from {args.data_file}")
        return

    # --- Load Model and Tokenizer ---
    model_name = "unsloth/Phi-3-mini-4k-instruct-bnb-4bit"
    max_seq_length = 2048
    dtype = None  # Autodetect

    model, tokenizer = FastLanguageModel.from_pretrained(
        model_name=model_name,
        max_seq_length=max_seq_length,
        dtype=dtype,
        load_in_4bit=args.load_in_4bit,  # Allow toggling 4-bit loading
    )

    # --- Prepare and Split Dataset ---
    # Create an evaluation set (10%) to monitor for overfitting during training.
    train_data, eval_data = train_test_split(data, test_size=0.1, random_state=42)

    train_dataset = Dataset.from_list(train_data)
    eval_dataset = Dataset.from_list(eval_data)

    # Apply the chat template to both the training and evaluation datasets.
    train_dataset = train_dataset.map(
        lambda x: {"text": to_text(x, tokenizer)},
        remove_columns=train_dataset.column_names,
    )
    eval_dataset = eval_dataset.map(
        lambda x: {"text": to_text(x, tokenizer)},
        remove_columns=eval_dataset.column_names,
    )

    print("==============================================")
    print(
        f"Training examples: {len(train_dataset)}, Evaluation examples: {len(eval_dataset)}"
    )
    print("Dataset prepared and formatted using CHAT TEMPLATE.")
    print("==============================================")

    # --- Configure LoRA Adapters ---
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
        # Dropout is a regularization technique to prevent the model from overfitting.
        lora_dropout=0.1,
        bias="none",
        use_gradient_checkpointing="unsloth",
        random_state=3407,
    )

    # --- Set Up Trainer ---
    trainer = SFTTrainer(
        model=model,
        tokenizer=tokenizer,
        train_dataset=train_dataset,
        # Provide the evaluation dataset to the trainer.
        eval_dataset=eval_dataset,
        dataset_text_field="text",
        max_seq_length=max_seq_length,
        dataset_num_proc=2,
        args=TrainingArguments(
            per_device_train_batch_size=2,
            gradient_accumulation_steps=4,
            warmup_steps=10,
            num_train_epochs=args.epochs,
            learning_rate=2e-4,
            fp16=not torch.cuda.is_bf16_supported(),
            bf16=torch.cuda.is_bf16_supported(),
            logging_steps=10,
            optim="adamw_8bit",
            weight_decay=0.01,
            lr_scheduler_type="linear",
            seed=3407,
            output_dir="outputs",
            # Evaluate and save checkpoints at regular intervals.
            save_strategy="steps",
            eval_strategy="steps",
            save_steps=50,
            eval_steps=25,
            # Keep only the best-performing checkpoints to save disk space.
            save_total_limit=2,
            # Automatically load the best model at the end of training.
            load_best_model_at_end=True,
            dataloader_pin_memory=False,
            report_to="none",  # Change to "tensorboard" to log metrics
        ),
    )

    # --- Train the Model ---
    print("Starting model training...")
    trainer.train()
    print("Training complete!")

    # --- Interactive Testing ---
    interactive_test(model, tokenizer, device)

    # --- Merge Adapters and Save GGUF ---
    # Note: If you trained in 4-bit, this step de-quantizes the model.
    # The save/reload cycle is required to ensure the model is in a clean 16-bit
    # format that the GGUF conversion script understands.
    if args.load_in_4bit:
        print("Merging LoRA adapters and de-quantizing model for GGUF conversion...")
        model = model.merge_and_unload()

        temp_merged_dir = "merged_model_for_gguf"
        model.save_pretrained(temp_merged_dir, safe_serialization=True)
        tokenizer.save_pretrained(temp_merged_dir)

        # Reload the model in 16-bit to ensure it's clean
        model, tokenizer = FastLanguageModel.from_pretrained(
            model_name=temp_merged_dir,
            max_seq_length=max_seq_length,
            dtype=dtype,
            load_in_4bit=False,
        )
        print("Model successfully de-quantized.")
    else:
        # If not trained in 4-bit, the process is simpler.
        print("Merging LoRA adapters...")
        model = model.merge_and_unload()
        print("Adapters merged.")

    print(f"Saving model to GGUF format ({args.quant_method})...")
    output_directory = "gguf_model"

    model.save_pretrained_gguf(
        output_directory,
        tokenizer,
        quantization_method=args.quant_method,
    )

    # Find the saved GGUF file to show the final path to the user
    gguf_files = [f for f in os.listdir(output_directory) if f.endswith(".gguf")]
    if gguf_files:
        final_path = os.path.join(output_directory, gguf_files[0])
        print("\n\n==============================================")
        print("  ✅ Model saved successfully! ")
        print(f"   -> GGUF file location: {final_path}")
        print("==============================================")
        print(
            "You can now import this file into Ollama or other GGUF-compatible runners."
        )
    else:
        print("Error: GGUF file was not created.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Fine-tune and save a language model.")
    parser.add_argument(
        "--data_file",
        type=str,
        default="data.json",
        help="Path to the training data JSON file.",
    )
    parser.add_argument(
        "--epochs", type=int, default=1, help="Number of training epochs."
    )
    parser.add_argument(
        "--quant_method",
        type=str,
        default="q4_k_m",
        help="The quantization method for the GGUF file (e.g., 'q4_k_m', 'q8_0').",
    )
    parser.add_argument(
        "--load_in_4bit",
        action="store_true",
        help="Load the base model in 4-bit for memory-efficient training (QLoRA).",
    )
    args = parser.parse_args()
    main(args)

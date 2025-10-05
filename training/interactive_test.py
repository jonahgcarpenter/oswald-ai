def interactive_test(model, tokenizer):
    """
    Creates an interactive loop to test the model with user prompts.
    """
    print("\n--- Interactive Model Test ---")
    print("Enter a prompt to test the fine-tuned model.")
    print("Type 'save' to finish testing and save the GGUF model.")
    print("Type 'cancel' to exit without saving.")
    print("------------------------------------")

    while True:
        # Get input from the user
        user_input = input("\nPrompt: ")

        # Check for control commands
        if user_input.lower() == "save":
            print("\nProceeding to save the model...")
            return True  # Signal to continue and save
        elif user_input.lower() == "cancel":
            print("\nCanceling save. Exiting script.")
            return False  # Signal to exit without saving

        # Prepare the input for the model
        messages = [
            {"role": "user", "content": user_input},
        ]
        inputs = tokenizer.apply_chat_template(
            messages,
            tokenize=True,
            add_generation_prompt=True,
            return_tensors="pt",
        ).to("cuda")

        # Generate a response
        outputs = model.generate(input_ids=inputs, max_new_tokens=256, use_cache=True)
        response_text = tokenizer.batch_decode(outputs, skip_special_tokens=True)[0]

        # Clean up the output to only show the assistant's part
        assistant_response = response_text.split("<|assistant|>")
        if len(assistant_response) > 1:
            clean_response = assistant_response[1].strip()
        else:
            clean_response = response_text  # Fallback if format is different

        print(f"Model: {clean_response}")

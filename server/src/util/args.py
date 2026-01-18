import argparse


def get_server_config():
    """
    Parses command-line arguments for the server configuration.
    Returns:
        argparse.Namespace: The parsed arguments.
    """
    parser = argparse.ArgumentParser(description="Run the Oswald AI server")

    parser.add_argument(
        "--host",
        type=str,
        default="127.0.0.1",
        help="Host IP (default: 127.0.0.1)",
    )
    parser.add_argument("--port", type=int, default=8000, help="Port (default: 8000)")
    parser.add_argument("--reload", action="store_true", help="Enable auto-reload")

    return parser.parse_args()

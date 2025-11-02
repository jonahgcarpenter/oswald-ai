import uvicorn
from fastapi import FastAPI

app = FastAPI()


# TODO: Turn into healthcheck
@app.get("/")
def read_root():
    """
    This is the function that handles requests to the "/" URL.
    """
    return {"message": "Hello, World!"}


if __name__ == "__main__":
    """
    Starts the Uvicorn server.
    """
    uvicorn.run(
        "main:app",
        host="127.0.0.1",
        port=8000,
        reload=True,
    )

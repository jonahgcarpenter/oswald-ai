FROM python:3.13-slim-bookworm as builder

ENV PYTHONDONTWRITEBYTECODE 1
ENV PYTHONUNBUFFERED 1

WORKDIR /opt/venv

RUN python -m venv .

COPY requirements.txt .

RUN . /opt/venv/bin/activate && pip install --no-cache-dir -r requirements.txt

FROM python:3.13-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE 1
ENV PYTHONUNBUFFERED 1

RUN useradd --system -U oswald-ai

WORKDIR /home/oswald-ai/app

COPY --from=builder /opt/venv /opt/venv

COPY ./base ./base
COPY ./tools ./tools
COPY ./main.py ./

RUN chown -R oswald-ai:oswald-ai /home/oswald-ai /opt/venv

# Set the user to the non-root user
USER oswald-ai

ENV PATH="/opt/venv/bin:$PATH"

CMD ["python", "main.py"]

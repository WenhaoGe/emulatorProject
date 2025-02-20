FROM python:3.7-alpine
COPY requirements.txt /
RUN apk add gcc linux-headers libc-dev libffi-dev py-cffi python3-dev musl-dev make &&\
    pip install --upgrade pip setuptools wheel && \
    pip install -r /requirements.txt && \
    apk del gcc linux-headers libc-dev libffi-dev py-cffi musl-dev make
COPY src /app
WORKDIR /app
CMD ["python", "redfishMockupServer.py", "-H" , "0.0.0.0"]

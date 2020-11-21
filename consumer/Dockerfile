FROM python:3.7-slim

WORKDIR /app
COPY requirements.txt /app
RUN pip install -Iv eventlet==0.25.1 flask-socketioflask-socketio==4.2.1 redis==3.3.11

COPY . /app
ENTRYPOINT [ "python", "-u", "z/app/consumer.py" ]
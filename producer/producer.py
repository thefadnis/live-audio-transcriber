import argparse,queue,eventlet,redis
from flask import Flask, render_template
from flask_socketio import SocketIO
eventlet.monkey_patch()

health_check_interval = 2
# connected_count = 0
q = queue.Queue()
app = Flask(__name__)

parser = argparse.ArgumentParser()
parser.add_argument('--host', default='localhost')
parser.add_argument('--port', default=8080)
parser.add_argument('--redisHost', required=True)
parser.add_argument('--redisQueue', default='transcriptions')
parser.add_argument('--id', default='producer')
params = parser.parse_args()

socketio = SocketIO(ping_timeout=5, ping_interval=2)
redisDB = redis.Redis(host=params.redisHost, port=6379, db=0, socket_timeout=3,
                  health_check_interval=health_check_interval)

def _qread():
    while True:
        while not q.empty():
            try:
                transcript_part = redisDB.brpop(params.redisQueue, timeout=2)
                if transcript_part is not None:
                    socketio.emit('transcript', transcript_part[1].decode('utf-8'))
            except redis.exceptions.ReadOnlyError as re:
                print('Redis Read Error (failover?): %s' % re)
                socketio.emit('transcript', '[REDIS-FAILOVER]')
                socketio.sleep(health_check_interval)
            except redis.exceptions.RedisError as err:
                print('RedisError: %s' % err)
        socketio.sleep(0.2)

@app.route('/')
def index():
    return render_template('index.html', async_mode=socketio.async_mode)


@socketio.on('connect')
def connect():
    print('Connected : %s' % params.id)
    socketio.emit('pod_id', params.id)
    buff.put_nowait(1)


@socketio.on('disconnect')
def disconnect():
    print('Disconnected : %s' % params.id)
    buff.get_nowait()

if __name__ == '__main__':
    print('Started %s...' % params.id)
    socketio.init_app(app)
    socketio.start_background_task(_qread)
    socketio.run(app, host=params.host, port=params.port)
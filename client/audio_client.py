import argparse
from datetime import datetime
import wave

import eventlet
import socketio

eventlet.monkey_patch()
sio = socketio.Client(reconnection_delay=1, reconnection_delay_max=1,
                      randomization_factor=0, logger=False)


@sio.event
def connect():
    print('Socket connected at %s' % datetime.utcnow())


@sio.event
def disconnect():
    print('Socket disconnected at %s' % datetime.utcnow())


@sio.on('pod_id')
def pod_id(msg):
    print('Connected to pod: %s' % msg)


def stream_file(filename):
    audio_binary = wave.open(filename, 'rb')
    # read in ~100ms chunks
    audio_part = int(audio_binary.getframerate() / 10)
    data = audio_binary.readframes(audio_part)

    try:
        while sio.connected:
            if data != '' and len(data) != 0:
                sio.emit('data', data)
                # sleep for the duration of the audio chunk
                # to mimic real time playback
                sio.sleep(0.1)
                data = audio_binary.readframes(audio_part)
            else:
                print('file ended, exiting')
                sio.sleep(0.5)
                break
                # wf = wave.open(filename, 'rb')
                # data = wf.readframes(chunk)
                # print('restarting playback')
        sio.sleep(0.2)
    except socketio.exceptions.ConnectionError as err:
        print('Connection error: %s! Retrying at %s' %(err, datetime.utcnow()))
    except KeyboardInterrupt:
        return


try:
    parser = argparse.ArgumentParser()
    parser.add_argument('--targetip', default='localhost:8080')
    parser.add_argument('--file', default='humptydumpty.wav')
    args = parser.parse_args()

    url = 'http://' + args.targetip
    sio.connect(url)
    stream_file(args.file)
except KeyboardInterrupt:
    pass

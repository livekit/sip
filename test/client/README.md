# client

This is a SIP client that sends an INVITE and then sends PCMU audio for a local file. It is useful to test the `sip` server.

### Running

At this time no options exist, `go run .` is all that is required. It connects to a SIP server running on the local machine and will send audio.

This program expects a `audio.mkv` to be in the directory. The file should contain PCM s16le in 20ms samples with a sample rate of 8000 and one channel.
You can use the command below to convert a file, or convert [this](https://siobud.com/audio.mkv) one.


### Converting an audio file

`ffmpeg -i $AUDIO_FILE -c:a pcm_s16le audio.mkv`

`gst-launch-1.0 filesrc location=$AUDIO_FILE ! decodebin ! audiomixer output-buffer-duration=20000000 ! audiorate ! audioresample ! audioconvert ! audio/x-raw, format=S16LE, rate=8000, channels=1 ! matroskamux ! filesink location=audio.mkv`

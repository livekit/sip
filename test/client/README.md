# client

This is a SIP client that sends an INVITE and then sends PCMU audio for a local file. It is useful to test the `sip` server.

### Running

At this time no options exist, `go run .` is all that is required. It connects to a SIP server running on the local machine and will send audio.

This program expects a `audio.mkv` to be in the directory. The file should contain PCMU in 20ms samples with a samplerate of 8000 and one channel.


### Creating a `audio.mkv` with GStreamer

`gst-launch-1.0 filesrc location=$AUDIO_FILE ! decodebin ! audiomixer output-buffer-duration=20000000 ! audiorate ! audioresample ! audioconvert ! audio/x-raw, rate=8000, channels=1 ! mulawenc ! matroskamux ! filesink location=audio.mkv`

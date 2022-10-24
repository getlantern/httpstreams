httpstreams

simple backend for handing http/2 and http/3 streams as a transport.
This relies on implementations allowing streams to be used bidirectionally
simultaneously, which is somewhat fragile (not guaranteed) but workable in many implementations.  some grpc implementations appear to rely on this.

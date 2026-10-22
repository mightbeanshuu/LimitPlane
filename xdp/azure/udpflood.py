# 20,000 small UDP packets at one target, as fast as one socket can send.
import socket, sys, time
target = sys.argv[1]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
t = time.time()
for _ in range(20000):
    s.sendto(b"x" * 32, (target, 9))
print("sent 20000 UDP packets in %.2fs" % (time.time() - t))

# Left-Right Map in Go

This is experimental code for educational purpose.  Do not use in production.

Seriously, DO NOT USE THIS CODE anywhere near production or even at all!  How
concurrency characteristics of this code relate to the memory model is not
clear.

Inspired by Jon Gjengset's [left-right](https://github.com/jonhoo/left-right).

While trying to learn Rust I've watched [Jon's stream](https://www.youtube.com/watch?v=eLNAMEoKAAc)
on how he refactored his [rust-evmap](https://github.com/jonhoo/rust-evmap)
into a generic version, then named left-right.  This made me want to see in how
few lines one would be able to make a reasonable implementation in
(non-generic) Go.  Also, I plan to use this project as a toy for learning the
upcoming Type Parameters (Generics) in Go.

Apparently there is prior work by Pedro Ramalhete and Andreia Correia (2013):
"Left Right: A Concurrency Control Technique with Wait-Free Population
Oblivious Reads" [blog post](https://concurrencyfreaks.blogspot.com/2013/12/left-right-concurrency-control.html),
[preprint paper (pdf)](https://iweb.dl.sourceforge.net/project/ccfreaks/papers/LeftRight/leftright-extended.pdf)
(Warning: Sourceforge).

## What is Left-Right?

A left-right collection (in this experimental implementation: map[int]int, but
could be any collection type) is an attempt to eliminate locks entirely from
hot paths in concurrent applications where read operations are massive and
write operations relatively rare.  Above authors show that in their
implementations reads scale linearly with the number of readers.

### A metaphorical explanation:

There are two "arenas" (left, right).  When a reader "enters", the reader reads
a sign (an atomically read pointer), that points to which arena they should go.
Once entered an arena, the reader is welcome to do as many reads as they please
while the arena presents a consistent read-only state.  Meanwhile, the writers
are free to do any write operations on the other arena, undisturbed by the
presence of readers.  Every write operation is also appended to an operations
log (like a redo log, write ahead log).  Once the writer is done with its write
operation, the "sign" is swapped and any reader that enters, goes to the
recently-written-to arena.  The writer waits until all readers that were still
in the other arena to leave.  Once the writer is convinced all readers have
left the other arena, all operation from the log are re-done in order to sync
up both arenas to the same state.  Thence the writer can continue doing write
operations.  Rinse, repeat.

The writer does synchronize any actions with a central mutex lock.  Keys and
values are kept at most three times:  Once for both arenas, plus any
non-flushed value in the operations log.  The writer is in control over how
long that operations log is:  It is truncated after each flush operation.  The
delay of each flush is dependent on the readers:  They need to preemptively
leave the arena to allow the writer to carry out the flush operation.

The "sign", which is in fact a simple pointer, is written by the writer and
read by the readers with atomic operations.  The signaling of where the readers
are is done with an unsigned integer counter (named "epoch") that is atomically
incremented by 1 when entering an arena and when leaving an arena.  Thus, if a
reader has entered an arena the epoch is odd, and even if the reader has left
the arenas.  The writer atomically reads each epoch and spins over every epoch
that happened to be odd at that first read until all of those epochs differ
from the recorded odd epoch.   That ensures that every reader has at least once
read the pointer after the writer swapped it (and also handles the case of
overflowed epochs).

### Performance

Once a reader has entered, all reads are as cheap as normal native reads from
the collection (map in this case) which is as fast as it gets.  So fast in
fact, that in my early tests all the read operations where inlined in tight
loops, such that Go's scheduler was unable to yield to other goroutines (see
`runtime.Gosched()`).

However, readers need to preempt by regularly `Enter()`'ing and `Leave()`'ing
which---although does not encounter locks---is not free, because atomic
operations impact cache lines.  It is entirely possible that `sync.RWLock`
performs similarly, thus benchmarking the actual use cases is obligatory.
`sync.RWLock` does not allow simultaneous reading/writing and concurrency of
reads drains down to zero when a writer wants to read, though.

Creating readers is relatively expensive.  Each reader registers itself in the
writer (which involves locks), because the writer must observe if any readers
are in the live arena.  If this algorithm is e.g. considered in HTTP handlers,
each request is handled in its own Goroutine and thus also needs an own reader.
Creating one reader for each request quickly destroys any benefit.  This
implementation offers a pool of reusable readers to alleviate this bottleneck,
but if that in total provides any benefit can only be answered with use case
specific benchmarks.

I did not perform systematic benchmarks myself.


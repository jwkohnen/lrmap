# Left-Right Map in Go

This is experimental code for educational purpose.  Do not use in production.

Inspired by Jon Gjengset's [left-right](https://github.com/jonhoo/left-right).

While trying to learn Rust I've watched [Jon's stream](https://www.youtube.com/watch?v=eLNAMEoKAAc)
on how he refactored his [rust-evmap](https://github.com/jonhoo/rust-evmap)
into a generic version, then named left-right.  This made me want to see in how
few lines one would be able to make a reasonable implementation in
(non-generic) Go.  Also I plan to use this project as a toy for learning the
upcoming Type Parameters (Generics) in Go.

Apparently there is prior work by Pedro Ramalhete and Andreia Correia (2013):
"Left Right: A Concurrency Control Technique with Wait-Free Population
Oblivious Reads" [blog post](https://concurrencyfreaks.blogspot.com/2013/12/left-right-concurrency-control.html),
[preprint paper (pdf)](https://iweb.dl.sourceforge.net/project/ccfreaks/papers/LeftRight/leftright-extended.pdf)
(Warning: Sourceforge).

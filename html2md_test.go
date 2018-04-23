package html2md

import (
	"fmt"
	"os"
	"testing"
)

// Test: https://segment.com/blog/allocation-efficiency-in-high-performance-go-services/

func TestParseHTMLtoMD(t *testing.T) {
	f, err := os.OpenFile("Allocation Efficiency in High-Performance Go Services.md", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		panic(err)
	}

	var title = "# Allocation Efficiency in High-Performance Go Services\n"
	n, err := f.Write([]byte(title))
	if len(title) == n && err != nil {
		panic(err)
	}

	n, err = f.Write([]byte(ParseHTMLtoMD(testString, func(err interface{}) {
		fmt.Println(err)
	})))
	if len(testString) == n && err != nil {
		panic(err)
	}
}

var testString = `<p>Memory management can be <em>tricky</em>, to say the least. However, after reading <em>the literature</em>, one might be led to believe that all the problems are solved: sophisticated automated systems that manage the lifecycle of memory allocation free us from these burdens. </p><p>However, if you’ve ever tried to tune the garbage collector of a JVM program or optimized the allocation pattern of a Go codebase, you understand that this is far from a solved problem. Automated memory management helpfully rules out a large class of errors, <em>but that’s only half the story.</em> The hot paths of our software must be built in a way that these systems can work efficiently.</p><p>We found inspiration to share our learnings in this area while building a high-throughput service in Go called <em>Centrifuge</em>, which processes hundreds of thousands of events per second. Centrifuge is a critical part of Segment’s infrastructure. Consistent, predictable behavior is a requirement. Tidy, efficient, and precise use of memory is a major part of achieving this consistency.</p><p>In this post we’ll cover common patterns that lead to inefficiency and production surprises related to memory allocation as well as practical ways of blunting or eliminating these issues. We’ll focus on the key mechanics of the allocator that provide developers a way to get a handle on their memory usage.</p><h2 id="tools-of-the-trade">Tools of the Trade</h2><p>Our first recommendation is to <strong>avoid premature optimization</strong>. Go provides excellent profiling tools that can point directly to the allocation-heavy portions of a code base. There’s no reason to reinvent the wheel, so instead of taking readers through it here, we’ll refer to <a href="https://blog.golang.org/profiling-go-programs">this excellent post</a> on the official Go blog. It has a solid walkthrough of using <code>pprof</code> for both CPU and allocation profiling. These are the same tools that we use at Segment to find bottlenecks in our production Go code, and should be the first thing you reach for as well.</p><p>Use data to drive your optimization!</p><h2 id="analyzing-our-escape">Analyzing Our Escape</h2><p>Go manages memory allocation automatically. This prevents a whole class of potential bugs, but it doesn’t completely free the programmer from reasoning about the mechanics of allocation. Since Go doesn’t provide a direct way to manipulate allocation, developers must understand the rules of this system so that it can be maximized for our own benefit.</p><p>If you remember one thing from this entire post, this would be it: <strong>stack allocation is cheap and heap allocation is expensive</strong>. Now let’s dive into what that actually means.</p><p>Go allocates memory in two places: a global heap for dynamic allocations and a local stack for each goroutine. Go prefers <a href="https://en.wikipedia.org/wiki/Stack-based_memory_allocation">allocation on the stack</a> — most of the allocations within a given Go program will be on the stack. It’s cheap because it only requires two CPU instructions: one to push onto the stack for allocation, and another to release from the stack.</p><p>Unfortunately not all data can use memory allocated on the stack. <strong>Stack allocation requires that the lifetime and memory footprint of a variable can be determined at compile time.</strong> Otherwise a <a href="https://en.wikipedia.org/wiki/Memory_management#HEAP">dynamic allocation onto the heap</a> occurs at runtime. <code>malloc</code> must search for a chunk of free memory large enough to hold the new value. Later down the line, the garbage collector scans the heap for objects which are no longer referenced. It probably goes without saying that it is <em>significantly</em> more expensive than the two instructions used by stack allocation.</p><p>The compiler uses a technique called <a href="https://en.wikipedia.org/wiki/Escape_analysis">e</a><a href="https://en.wikipedia.org/wiki/Escape_analysis"><em>scape </em></a><a href="https://en.wikipedia.org/wiki/Escape_analysis"><em>a</em></a><a href="https://en.wikipedia.org/wiki/Escape_analysis"><em>nalysis</em></a><em> </em>to choose between these two options.<em> </em>The basic idea is to do the work of garbage collection at compile time. The compiler tracks the scope of variables across regions of code. It uses this data to determine which variables hold to a set of checks that prove their lifetime is entirely knowable at runtime. If the variable passes these checks, the value can be allocated on the stack. If not, it is said to <em>escape</em>, and must be heap allocated.</p><p>The rules for escape analysis aren’t part of the Go language specification. For Go programmers, the most straightforward way to learn about these rules is experimentation. The compiler will output the results of the escape analysis by building with <code>go build -gcflags &#39;-m&#39;</code>. Let’s look at an example:</p><pre data-language="text"><code>package main

import &quot;fmt&quot;

func main() {
        x := 42
        fmt.Println(x)
}</code></pre><pre data-language="text"><code>$ go build -gcflags &#39;-m&#39; ./main.go
# command-line-arguments
./main.go:7: x escapes to heap
./main.go:7: main ... argument does not escape</code></pre><p>See here that the variable <code>x</code> “<em>escapes to the heap</em>,” which means it will be dynamically allocated on the heap at runtime. This example is a little puzzling. To human eyes, it is immediately obvious that <code>x</code> will not escape the <code>main()</code> function. The compiler output doesn’t explain why it thinks the value escapes. For more details, pass the <code>-m</code> option multiple times, which makes the output more verbose:</p><pre data-language="text"><code>$ go build -gcflags &#39;-m -m&#39; ./main.go
# command-line-arguments
./main.go:5: cannot inline main: non-leaf function
./main.go:7: x escapes to heap
./main.go:7:         from ... argument (arg to ...) at ./main.go:7
./main.go:7:         from *(... argument) (indirection) at ./main.go:7
./main.go:7:         from ... argument (passed to call[argument content escapes]) at ./main.go:7
./main.go:7: main ... argument does not escape</code></pre><p>Ah, yes! This shows that <code>x</code> escapes because it is passed to a function argument which escapes itself — <em>more on this later</em>.</p><p>The rules may continue to seem arbitrary at first, but after some trial and error with these tools, patterns do begin to emerge. For those short on time, here’s a list of some patterns we’ve found which typically cause variables to escape to the heap:</p><ul><li><p><strong>Sending pointers or values containing pointers to channels.</strong> At compile time there’s no way to know which goroutine will receive the data on a channel. Therefore the compiler cannot determine when this data will no longer be referenced.</p></li></ul><ul><li><p><strong>Storing pointers or values containing pointers in a slice.</strong> An example of this is a type like <code>[]*string</code>. This always causes the contents of the slice to escape. Even though the backing array of the slice may still be on the stack, the referenced data escapes to the heap.</p></li></ul><ul><li><p><strong>Backing arrays of slices that get reallocated because an </strong><strong><code>append</code></strong><strong> would exceed their capacity.</strong> In cases where the initial size of a slice is known at compile time, it will begin its allocation on the stack. If this slice’s underlying storage must be expanded based on data only known at runtime, it will be allocated on the heap.</p></li></ul><ul><li><p><strong>Calling methods on an interface type.</strong> Method calls on interface types are a <em>dynamic dispatch — </em>the actual concrete implementation to use is only determinable at runtime. Consider a variable <code>r</code> with an interface type of <code>io.Reader</code>. A call to <code>r.Read(b)</code> will cause both the value of <code>r</code> and the backing array of the byte slice <code>b</code> to <em>escape</em> and therefore be allocated on the heap.</p></li></ul><p>In our experience these four cases are the most common sources of <em>mysterious</em> dynamic allocation in Go programs. Fortunately there are solutions to these problems! Next we’ll go deeper into some concrete examples of how we’ve addressed memory inefficiencies in our production software.</p><h2 id="some-pointers">Some Pointers</h2><p>The rule of thumb is: <strong>pointers point to data allocated on the heap.</strong> Ergo, reducing the number of pointers in a program reduces the number of heap allocations. This is not an axiom, but we’ve found it to be the common case in real-world Go programs.</p><p>It has been our experience that developers become proficient and productive in Go without understanding the performance characteristics of values versus pointers. A common hypothesis derived from intuition goes something like this: <em>“copying values is expensive, so instead I’ll use a pointer.”</em> However, in many cases copying a value is much less expensive than the overhead of using a pointer. <em>“Why”</em> you might ask?</p><ul><li><p><strong>The compiler generates checks when dereferencing a pointer.</strong> The purpose is to avoid memory corruption by running <code>panic()</code> if the pointer is <code>nil</code>. This is extra code that must be executed at runtime. When data is passed by value, it cannot be <code>nil</code>.</p></li></ul><ul><li><p><strong>Pointers often have poor locality of reference.</strong> All of the values used within a function are collocated in memory on the stack. <a href="https://en.wikipedia.org/wiki/Locality_of_reference">Locality of reference</a> is an important aspect of efficient code. It dramatically increases the chance that a value is warm in CPU caches and reduces the risk of a miss penalty during <a href="https://en.wikipedia.org/wiki/Cache_prefetching">prefetching</a>.</p></li></ul><ul><li><p><strong>Copying objects within a cache line is the roughly equivalent to copying a single pointer.</strong> CPUs move memory between caching layers and main memory on cache lines of constant size. On x86 this is 64 bytes. Further, Go uses a technique called <a href="https://luciotato.svbtle.com/golangs-duffs-devices">Duff’s device</a> to make common memory operations like copies very efficient.</p></li></ul><p>Pointers should primarily be used to reflect ownership semantics and mutability. In practice, the use of pointers to avoid copies should be infrequent. Don’t fall into the trap of premature optimization. It’s good to develop a habit of passing data by value, only falling back to passing pointers when necessary. An extra bonus is the increased safety of eliminating <code>nil</code>.</p><p>Reducing the number of pointers in a program can yield another helpful result as <strong>the garbage collector will skip regions of memory that it can prove will contain no pointers</strong>. For example, regions of the heap which back slices of type <code>[]byte</code> aren’t scanned at all. This also holds true for arrays of struct types that don’t contain any fields with pointer types.</p><p>Not only does reducing pointers result in less work for the garbage collector, it produces more cache-friendly code. Reading memory moves data from main memory into the CPU caches. Caches are finite, so some other piece of data must be evicted to make room. Evicted data may still be relevant to other portions of the program. The resulting <a href="https://pomozok.wordpress.com/2011/11/29/cpu-cache-thrashing/">cache thrashing</a> can cause unexpected and sudden shifts the behavior of production services.</p><h2 id="digging-for-pointer-gold">Digging for Pointer Gold</h2><p>Reducing pointer usage often means digging into the source code of the types used to construct our programs. Our service, Centrifuge, retains a queue of failed operations to retry as a circular buffer with a set of data structures that look something like this:</p><pre data-language="rust"><code>type retryQueue struct {
    buckets       [][]retryItem // each bucket represents a 1 second interval
    currentTime   time.Time
    currentOffset int
}

type retryItem struct {
    id   ksuid.KSUID // ID of the item to retry
    time time.Time   // exact time at which the item has to be retried
}</code></pre><p>The size of the outer array in <code>buckets</code> is constant, but the number of items in the contained <code>[]retryItem</code> slice will vary at runtime. The more retries, the larger these slices will grow. </p><p>Digging into the implementation details of each field of a <code>retryItem</code>, we learn that <a href="https://godoc.org/github.com/segmentio/ksuid#KSUID"><code>KSUID</code></a> is a type alias for <code>[20]byte</code>, which has no pointers, and therefore can be ruled out. <code>currentOffset</code> is an <code>int</code>, which is a fixed-size primitive, and can also be ruled out. Next, looking at the implementation of <code>time.Time</code> type[1]:</p><pre data-language="go"><code>type Time struct {
    sec  int64
    nsec int32
    loc  *Location // pointer to the time zone structure
}</code></pre><p>The <code>time.Time</code> struct contains an internal pointer for the <code>loc</code> field. Using it within the <code>retryItem</code> type causes the GC to <em>chase</em> the pointers on these structs each time it passes through this area of the heap.</p><p>We’ve found that this is a typical case of cascading effects under unexpected circumstances. During normal operation failures are uncommon. Only a small amount of memory is used to store retries. When failures suddenly spike, the number of items in the retry queue can increase by thousands per second, bringing with it a significantly increased workload for the garbage collector.</p><p>For this particular use case, the timezone information in <code>time.Time</code> isn’t necessary. These timestamps are kept in memory and are never serialized. Therefore these data structures can be refactored to avoid this type entirely:</p><pre data-language="go"><code>type retryItem struct {
    id   ksuid.KSUID
    nsec uint32
    sec  int64
}

func (item *retryItem) time() time.Time {
    return time.Unix(item.sec, int64(item.nsec))
}

func makeRetryItem(id ksuid.KSUID, time time.Time) retryItem {
    return retryItem{
        id:   id,
        nsec: uint32(time.Nanosecond()),
        sec:  time.Unix(),
}</code></pre><p>Now the <code>retryItem</code> doesn’t contain any pointers. This dramatically reduces the load on the garbage collector as the entire footprint of <code>retryItem</code> is knowable at compile time[2].</p><h2 id="pass-me-a-slice">Pass Me a Slice</h2><p>Slices are fertile ground for inefficient allocation behavior in hot code paths. Unless the compiler knows the size of the slice at compile time, the backing arrays for slices (and maps!) are allocated on the heap. Let’s explore some ways to keep slices on the stack and avoid heap allocation.</p><p>Centrifuge uses MySQL intensively. Overall program efficiency depends heavily on the efficiency of the MySQL driver. After using <code>pprof</code> to analyze allocator behavior, we found that the code which serializes <code>time.Time</code> values in Go’s MySQL driver was particularly expensive.</p><p>The profiler showed a large percentage of the heap allocations were in code that serializes a <code>time.Time</code> value so that it can be sent over the wire to the MySQL server.</p><figure><img src="https://assets.contents.io/asset_ognsdc07.png"/></figure><p>This particular code was calling the <code>Format()</code> method on <code>time.Time</code>, which returns a <code>string</code>. Wait, <em>aren’t we talking about slices?</em> Well, <a href="https://blog.golang.org/slices">according to the official Go blog</a>, a <code>string</code> is just a “read-only slices of bytes with a bit of extra syntactic support from the language.” Most of the same rules around allocation apply!</p><p>The profile tells us that a massive <strong>12.38%</strong> of the allocations were occurring when running this <code>Format</code> method. What does <code>Format</code> do?</p><figure><img src="https://assets.contents.io/asset_VQwlrhJK.png"/></figure><p>It turns out there is a much more efficient way to do the same thing that uses a common pattern across the standard library. While the <code>Format()</code> method is easy and convenient, code using <code>AppendFormat()</code> can be much easier on the allocator. Peering into the source code for the <code>time</code> package, we notice that all internal uses are <code>AppendFormat()</code> and not <code>Format()</code>. This is a pretty strong hint that <code>AppendFormat()</code> is going to yield more performant behavior.</p><figure><img src="https://assets.contents.io/asset_9IBZXwuO.png"/></figure><p>In fact, the <code>Format</code> method just wraps the <code>AppendFormat</code> method:</p><pre data-language="go"><code>func (t Time) Format(layout string) string {
          const bufSize = 64
          var b []byte
          max := len(layout) + 10
          if max &lt; bufSize {
                  var buf [bufSize]byte
                  b = buf[:0]
          } else {
                  b = make([]byte, 0, max)
          }
          b = t.AppendFormat(b, layout)
          return string(b)
}</code></pre><p>Most importantly, <code>AppendFormat()</code> gives the programmer far more control over allocation. It requires passing the slice to mutate rather than returning a string that it allocates internally like <code>Format()</code>. Using <code>AppendFormat()</code> instead of <code>Format()</code> allows the same operation to use a fixed-size allocation[3] and thus is eligible for stack placement.</p><p>Let’s look at the change we upstreamed to Go’s MySQL driver in <a href="https://github.com/go-sql-driver/mysql/pull/615">this PR</a>.</p><figure><img src="https://assets.contents.io/asset_41Pq2hXv.png"/></figure><p>The first thing to notice is that <code>var a [64]byte</code> is a fixed-size array. Its size is known at compile-time and its use is scoped entirely to this function, so we can deduce that this will be allocated on the stack.</p><p>However, this type can’t be passed to <code>AppendFormat()</code>, which accepts type <code>[]byte</code>. Using the <code>a[:0]</code> notation converts the fixed-size array to a slice type represented by <code>b</code> that is backed by this array. This will pass the compiler’s checks and be allocated on the stack.</p><p>Most critically, the memory that would otherwise be dynamically allocated is <em>passed</em> to <code>AppendFormat()</code>, a method which itself passes the compiler’s stack allocation checks. In the previous version, <code>Format()</code> is used, which contains allocations of sizes that can’t be determined at compile time and therefore do not qualify for stack allocation.</p><p>The result of this relatively small change massively reduced allocations in this code path! Similar to using the “Append pattern” in the MySQL driver, an <code>Append()</code> method was added to the <code>KSUID</code> type in <a href="https://github.com/segmentio/ksuid/pull/10">this PR</a>. Converting our hot paths to use <code>Append()</code> on <code>KSUID</code> against a fixed-size buffer instead of the <code>String()</code> method saved a similarly significant amount of dynamic allocation. Also noteworthy is that the <code>strconv</code> package has equivalent append methods for converting strings that contain numbers to numeric types.</p><h2 id="interface-types-and-you">Interface Types and You</h2><p>It is fairly common knowledge that method calls on interface types are more expensive than those on struct types. Method calls on interface types are executed via <a href="https://en.wikipedia.org/wiki/Dynamic_dispatch">dynamic dispatch</a>. This severely limits the ability for the compiler to determine the way that code will be executed at runtime. So far we’ve largely discussed shaping code so that the compiler can understand its behavior best at compile-time. Interface types throw all of this away!</p><p>Unfortunately interface types are a very useful abstraction — they let us write more flexible code. A common case of interfaces being used in the hot path of a program is the hashing functionality provided by standard library’s <code>hash</code> package. The <code>hash</code> package defines a set of generic interfaces and provides several concrete implementations. Let’s look at an example:</p><pre data-language="go"><code>package main

import (
        &quot;fmt&quot;
        &quot;hash/fnv&quot;
)

func hashIt(in string) uint64 {
        h := fnv.New64a()
        h.Write([]byte(in))
        out := h.Sum64()
        return out
}

func main() {
        s := &quot;hello&quot;
        fmt.Printf(&quot;The FNV64a hash of &#39;%v&#39; is &#39;%v&#39;\n&quot;, s, hashIt(s))
}</code></pre><p>Building this code with escape analysis output yields the following:</p><pre data-language="less"><code>./foo1.go:9:17: inlining call to fnv.New64a
./foo1.go:10:16: ([]byte)(in) escapes to heap
./foo1.go:9:17: hash.Hash64(&amp;fnv.s·2) escapes to heap
./foo1.go:9:17: &amp;fnv.s·2 escapes to heap
./foo1.go:9:17: moved to heap: fnv.s·2
./foo1.go:8:24: hashIt in does not escape
./foo1.go:17:13: s escapes to heap
./foo1.go:17:59: hashIt(s) escapes to heap
./foo1.go:17:12: main ... argument does not escape</code></pre><p>This means the <code>hash</code> object, input string, and the <code>[]byte</code> representation of the input will all escape to the heap. To human eyes these variables obviously do not escape, but the interface type ties the compilers hands. And there’s no way to safely use the concrete implementations without going through the <code>hash</code> package’s interfaces. So what is an efficiency-concerned developer to do?</p><p>We ran into this problem when constructing Centrifuge, which performs non-cryptographic hashing on small strings in its hot paths. So we built the <a href="https://github.com/segmentio/fasthash"><code>fasthash</code></a><a href="https://github.com/segmentio/fasthash"> library as an answer</a>. It was straightforward to build — the code that does the hard work is part of the standard library. <code>fasthash</code> just repackages the standard library code with an API that is usable without heap allocations.</p><p>Let’s examine the <code>fasthash</code> version of our test program:</p><pre data-language="go"><code>package main

import (
        &quot;fmt&quot;
        &quot;github.com/segmentio/fasthash/fnv1a&quot;
)

func hashIt(in string) uint64 {
        out := fnv1a.HashString64(in)
        return out
}

func main() {
        s := &quot;hello&quot;
        fmt.Printf(&quot;The FNV64a hash of &#39;%v&#39; is &#39;%v&#39;\n&quot;, s, hashIt(s))
}</code></pre><p>And the escape analysis output?</p><pre data-language="less"><code>./foo2.go:9:24: hashIt in does not escape
./foo2.go:16:13: s escapes to heap
./foo2.go:16:59: hashIt(s) escapes to heap
./foo2.go:16:12: main ... argument does not escape</code></pre><p>The only remaining escapes are due to the dynamic nature of the <code>fmt.Printf()</code> function. While we’d strongly prefer to use the standard library from an ergonomics perspective, in some cases it is worth the trade-off to go to such lengths for allocation efficiency.</p><h2 id="one-weird-trick">One Weird Trick</h2><p>Our final anecdote is more amusing than practical. However, it is a useful example for understanding the mechanics of the compiler’s escape analysis. When reviewing the standard library for the optimizations covered, we came across a rather curious piece of code.</p><pre data-language="go"><code>// noescape hides a pointer from escape analysis.  noescape is
// the identity function but escape analysis doesn&#39;t think the
// output depends on the input.  noescape is inlined and currently
// compiles down to zero instructions.
// USE CAREFULLY!
//go:nosplit
func noescape(p unsafe.Pointer) unsafe.Pointer {
    x := uintptr(p)
    return unsafe.Pointer(x ^ 0)
}</code></pre><p>This function will hide the passed pointer from the compiler’s escape analysis functionality. What does this actually mean though? Well, let’s set up an experiment to see!</p><pre data-language="go"><code>package main

import (
        &quot;unsafe&quot;
)

type Foo struct {
        S *string
}

func (f *Foo) String() string {
        return *f.S
}

type FooTrick struct {
        S unsafe.Pointer
}

func (f *FooTrick) String() string {
        return *(*string)(f.S)
}

func NewFoo(s string) Foo {
        return Foo{S: &amp;s}
}

func NewFooTrick(s string) FooTrick {
        return FooTrick{S: noescape(unsafe.Pointer(&amp;s))}
}

func noescape(p unsafe.Pointer) unsafe.Pointer {
        x := uintptr(p)
        return unsafe.Pointer(x ^ 0)
}

func main() {
        s := &quot;hello&quot;
        f1 := NewFoo(s)
        f2 := NewFooTrick(s)
        s1 := f1.String()
        s2 := f2.String()
}</code></pre><p>This code contains two implementations that perform the same task: they hold a string and return the contained string using the <code>String()</code> method. However, the escape analysis output from the compiler shows us that the <code>FooTrick</code> version does not escape!</p><pre data-language="less"><code>./foo3.go:24:16: &amp;s escapes to heap
./foo3.go:23:23: moved to heap: s
./foo3.go:27:28: NewFooTrick s does not escape
./foo3.go:28:45: NewFooTrick &amp;s does not escape
./foo3.go:31:33: noescape p does not escape
./foo3.go:38:14: main &amp;s does not escape
./foo3.go:39:19: main &amp;s does not escape
./foo3.go:40:17: main f1 does not escape
./foo3.go:41:17: main f2 does not escape</code></pre><p>These two lines are the most relevant:</p><pre data-language="less"><code>./foo3.go:24:16: &amp;s escapes to heap
./foo3.go:23:23: moved to heap: s</code></pre><p>This is the compiler recognizing that the <code>NewFoo()</code> function takes a reference to the string and stores it in the struct, causing it to escape. However, no such output appears for the <code>NewFooTrick()</code> function. If the call to <code>noescape()</code> is removed, the escape analysis moves the data referenced by the <code>FooTrick</code> struct to the heap. What is happening here?</p><pre data-language="go"><code>func noescape(p unsafe.Pointer) unsafe.Pointer {
    x := uintptr(p)
    return unsafe.Pointer(x ^ 0)
}</code></pre><p>The <code>noescape()</code> function masks the dependency between the input argument and the return value. The compiler does not think that <code>p</code> escapes via <code>x</code> because the <code>uintptr()</code> produces a reference that is <em>opaque</em> to the compiler. The builtin <code>uintptr</code> type’s name may lead one to believe this is a bona fide pointer type, but from the compiler’s perspective it is just an integer that just happens to be large enough to store a pointer. The final line of code constructs and returns an <code>unsafe.Pointer</code> value from a seemingly arbitrary integer value. Nothing to see here folks!</p><p><code>noescape()</code> is used in dozens of functions in the <code>runtime</code> package that use <code>unsafe.Pointer</code>. It is useful in cases where the author knows for certain that data referenced by an <code>unsafe.Pointer</code> doesn’t escape, but the compiler naively thinks otherwise.</p><p>Just to be clear — we’re not recommending the use of such a technique. There’s a reason why the package being referenced is called <code>unsafe</code> and the source code contains the comment “USE CAREFULLY!”</p><h2 id="takeaways">Takeaways</h2><p>Building a state-intensive Go service that must be efficient and stable under a wide range of real world conditions has been a tremendous learning experience for our team. Let’s review our key learnings:</p><ol><li><p>Don’t prematurely optimize! Use data to drive your optimization work.</p></li><li><p>Stack allocation is cheap, heap allocation is expensive.</p></li><li><p>Understanding the rules of escape analysis allows us to write more efficient code.</p></li><li><p>Pointers make stack allocation mostly infeasible.</p></li><li><p>Look for APIs that provide allocation control in performance-critical sections of code.</p></li><li><p>Use interface types sparingly in hot paths.</p></li></ol><p>We’ve used these relatively straightforward techniques to improve our own Go code, and hope that others find these hard-earned learnings helpful in constructing their own Go programs.</p><p>Happy coding, fellow gophers!</p><h1 id="notes">Notes</h1><p>[1] The <code>time.Time</code> struct type has <a href="https://golang.org/src/time/time.go?s=5813:6814#L106">changed in Go 1.9</a>.</p><p>[2] You may have also noticed that we switched the order of the <code>nsec</code> and <code>sec</code> fields, the reason is that due to the alignment rules, Go would generate a 4 bytes padding after the KSUID. The nanosecond field happens to be 4 bytes so by placing it after the KSUID Go doesn’t need to add padding anymore because the fields are already aligned. This dropped the size of the data structure from 40 to 32 bytes, reducing by 20% the memory used by the retry queue.</p><p>[3] Fixed-size arrays in Go are similar to slices, but have their size encoded directly into their type signature. While most APIs accept slices and not arrays, slices can be made out of arrays!</p>`

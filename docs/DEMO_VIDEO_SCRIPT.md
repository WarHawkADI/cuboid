# Demo video script

About 2 minutes 40 seconds, roughly 370 words. One voice, calm and unhurried: the numbers do the selling.

## How to record the voice
- Read each section below, then stay **silent for 2 full seconds** before the next. The pauses let the video be cut to your voice.
- If you fluff a line, pause and say that whole section again. Keep going; the last take of a section is the one to use.
- Quiet room, phone or headset mic about a hand's width away. Save as one MP3.
- Say the numbers as written here. They come from `artifacts/`, so do not round them further.

## The script

### 1. The problem
*On screen: the recorder, with 1,000 dashboards on Direct. The line is high.*

> We're team iiitdards, and this is Cuboid. Ten million orders, a hundred stores, and a thousand dashboards, all on one laptop. Right now those dashboards are querying the main database directly. This line is the store app's own query time. It has climbed from about one millisecond to about a hundred. That's the problem in the PRD's own words: one big store's report makes the app slow for all stores.

### 2. The flip
*On screen: click Cuboid. The band changes and the line drops.*

> Same load, same database. We switch the dashboards to Cuboid. Dashboard queries on the main database drop to zero, and the app's query time falls back to about a millisecond. Cuboid reads the database's change stream and keeps exact rollups in a separate store. Dashboards never touch the main database.

### 3. The four queries
*On screen: store 42's page. Scroll from the summary to the trend, top products and status.*

> Here's the largest store. Orders and sales, a daily trend, top products, and a breakdown by status: the PRD's four queries. Uncached, they answer in under forty milliseconds, and every response says how fresh it is.

### 4. New data
*On screen: a new order appears, then moves to cancelled.*

> Add an order, and it shows up in about a second. Cancel it, and it moves from placed to cancelled.

### 5. Correct
*On screen: open a number. The strip shows how it was built. Press Check against main database, and the verdict appears.*

> But how do you know the numbers are right? Click any one. This shows how it was assembled: whole months, days and hours, plus two thin edges of raw rows, so any timestamp is exact. Now we check it against the main database, at the same point in the log, while writes keep running. Exact match. Across twenty-five hundred checks, every one matched. This same check once caught a bug that lost two orders, which unit tests could not see.

### 6. Separate
*On screen: the Isolation page. Run drill B. The row stops at the result check, and "0 leaked" appears.*

> INC-02 was a cache bug that showed users other users' data. So we inject that kind of bug into our own code. This drill hands requests the wrong rows, and the result check stops them. Zero leaked. A million randomized requests: zero crossed stores. And twenty hard kills under writes: back in about three seconds, and still exact.

### 7. The bonuses
*On screen: the flood result, then the new breakdown going live. The build is shortened, and a caption says so.*

> The biggest store floods heavy queries and gets limited, while other stores don't notice. One line of YAML adds a new breakdown, with no restart and no load on the main database. The same engine also serves SOC alerts, through config alone.

### 8. What we measured
*On screen: the targets table, then the Results page.*

> Here's what we measured. Freshness and correctness hit Great. Speed and database safety hit Good. And one query, top customers, misses the growth target, at nearly fourteen times. We show you that, because every number here comes from a file you can check.

### 9. Close
*On screen: the deck's cover.*

> Cuboid. Fast for every store. Exact to the paisa. Never in the wrong store's hands.

## What the video must not imply
- The YAML build takes about a minute at 10M. If it is shortened, a caption says so.
- The data, the store app's traffic and the dashboards' traffic are simulated. Replication, rollups, checks and injected faults are real.
- "About one millisecond" and "about a hundred" are the measured 1.16 ms and 104 ms. The live recording may read a little differently: it is live.

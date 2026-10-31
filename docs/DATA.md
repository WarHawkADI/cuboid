# Data

## Source
The organisers' data script has not been supplied, so `provided/` is empty and Cuboid uses its fallback generator, `cmd/seed`. It mirrors the PRD's entities. Everything specific to the schema lives in `config/metrics.yaml` and `migrations/main/001_schema.sql`; the engine reads table and column names from config and column types from the main database's catalog. Moving to the organisers' schema has not been done, so this portability is a design claim until it is.

## Tables
| Table | Holds | Notes |
|---|---|---|
| `companies` | company id, name | Not mirrored |
| `tenants` | store id, company id, name | A tenant is a store |
| `orders` | one product per order, quantity, amount in paise, status, payment method, channel, timestamps | The fact table. Single-product orders are a deliberate simplification |
| `products` | name, category, price | Category lives here, so a breakdown by category uses a join dimension |
| `customers` | name, email and phone (PII, never mirrored), city, region | |
| `soc_alerts` | severity, source product, MITRE ATT&CK tactic, status, asset, risk score | Mitigata's shape of data, on the same engine through config |

## Tiers
| Tier | Orders | Stores | Command |
|---|---|---|---|
| `dev` | 100,000 | 5 (ids 40-44) | `seed --tier dev` |
| `small` | 1,000,000 | 10 (ids 37-46) | `seed --tier small` |
| `main` | 10,000,000 | 100 (ids 1-100) | `seed --tier main` |

Customers number about a sixth of orders, products 200 to 3,000 per store, and SOC alerts a tenth of orders.

## Realism
- **Skew:** order counts follow a Zipf-like curve. In `main`, store 42 is the largest with about 19% of orders and the smallest store has about 19,000.
- **Time:** 24 months of history with growth, weekly and daily cycles, and a festive uplift from 10 October to 15 November.
- **Lifecycle:** recent orders are mid-lifecycle, so the store app can advance them. About 8% of older orders are cancelled and 3% returned.
- **Payments:** UPI-heavy.
- **Companies:** company 7 owns stores 42, 43 and one more; about a third of the others own 2 to 5 stores.
- **Names:** store, product and customer names are generated and obviously synthetic, such as `customer123@store42.example.invalid`.

## Things to know when reading the numbers
- The same seed and anchor produce identical data, and `seed --hash` prints a hash of every order row. An automated test of this does not exist yet.
- The store app defaults to about 48 new orders a second across all stores, against about 0.2 a second in the generated history. After a while "today" is much larger than a historic day, and charts show it as a spike. This is load for the test, not a model of real traffic.
- `seed` needs a superuser and truncates the tables. It sets a flag so Cuboid waits and then takes one snapshot.

# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users
- **Primary: engineers in a demo or reviewing the service afterwards.** They watch a live walkthrough or open the public link later, and judge how the service works and how well it is built.
- **Secondary: people who run or plan charging sites.** The numbers should read as credible to them: observed facts are labelled as observed, and estimates as estimates.

## Product Purpose
ev-bill bills EV charging sessions on a Singapore peak/off-peak tariff (peak 07:00–23:00 at 62¢/kWh, off-peak 23:00–07:00 at 48¢/kWh). It also mirrors the live occupancy of real public chargers from LTA DataMall into billed sessions. The status pages show those sessions as they happen. The Locations page shows whether public charging at the watched sites is under-used.

Success means a viewer leaves confident of two things: the system is live (real chargers are tracked right now), and the utilization figures are trustworthy.

## Positioning
The service bills real Singapore chargers, observed every 5 minutes from LTA DataMall, rather than a canned simulation. Utilization comes straight from the occupancy LTA publishes. Energy and money are clearly marked estimates, because the feed has no meter readings.

## Operating Context
- The pages are server-rendered Go `html/template` with no JS framework, plus one small same-origin script. They update live over Server-Sent Events.
- They are public and unauthenticated, hosted on Render's free tier in Singapore.
- They are opened on laptops during demos and on phones afterwards.

## Capabilities and Constraints
- **Watched sites:** postal codes 038983 (GV Suntec City), 519612 (Pasir Ris Mall) and 090032 (Blk 29/32 Telok Blangah Rise HDB car park), 68 connectors in all.
- **What the feed gives per connector:** occupancy status (occupied, available or not available, where occupied includes reserved and blocked), rated kW, AC/DC, plug type, operator, and the posted price including GST.
- **Estimates:** energy is estimated at rated power, with DC capped at 50 kW and 60 kWh per occupancy. Money shown on the Locations page is estimated energy billed at our 62/48 tariff, the same figure the site's sessions page shows; the operator's posted price appears only in the charger lines of a site's sessions page, since the feed gives only today's price.
- **Data window:** history is a rolling 14 days, starting from 8 November 2026.
- **Technical constraints:**
  - CSP `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'`;
  - no web frameworks, ORMs, JS frameworks or Docker;
  - money is integer cents, energy integer Wh.
- **Must work on a phone.**

## Evidence on Hand
- Live data from the mirror: real connector IDs, locations, operators and occupancy.
- No testimonials, customers, benchmarks or partner claims exist; do not invent any.
- The 62/48 tariff is the project's own billing rule, not a real operator's price.

## Product Principles
1. Observed and estimated are always visibly distinct.
2. Liveness should be evident without explanation.
3. Every number traces to a stated rule (BIL, SES, MIR or LIVE in the specs).
4. Restraint over decoration: this is an operating view, not a marketing page.

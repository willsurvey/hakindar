# 🧠 Logika Perbaikan Sinyal & Manajemen Risiko

*Kembali ke [Halaman Utama](README.md)*

Kodebase telah diperbaiki secara signifikan untuk menghasilkan sinyal trading yang lebih berkualitas tinggi. Pembaruan terbaru menggeser paradigma dari "menolak sinyal secara paksa" menjadi "membobot probabilitas sinyal".

## 1. Filter Pipeline Berbasis Statistik

Sistem `SignalFilter` (pada `app/signal_filter.go`) kini difokuskan pada pengolahan kelayakan berbasis angka.

### a. Gerbang Keselamatan Pasar (IHSG Safety Gate)
Dilengkapi dengan suplai data dari Python Screener via Redis, kini sistem akan membaca `Regime Pasar`.
- Jika pasar berada dalam fase `BEARISH` yang sangat dalam, sistem bisa memveto (membatalkan) sinyal `BUY` dari saham-saham individual untuk melindungi Anda dari arus jual besar-besaran (Market Crash Protection).

### b. Filter Kinerja Strategi (*Strategy Performance*)
- **Baseline Recency Check**: Mengutamakan data *baseline* (seperti VWAP & ATR) yang sangat *up-to-date*. Ini bisa terwujud karena sistem otomatis mengunduh 60 hari data intraday terbaru melalui fitur **Smart Bootstrap**.
- **Consecutive Losses Circuit Breaker**: Mengurangi *multiplier* (peluang lolos sinyal) apabila sebuah strategi terlalu sering mengalami kerugian beruntun.
- Alih-alih langsung membuang sinyal jika sebuah ambang batas (*threshold*) tak tercapai, sistem akan membiarkan sinyal lolos dengan **persentase kepercayaan (Confidence) yang dikurangi**.

### c. Filter Kepercayaan Dinamis (*Dynamic Confidence*)
- **Ambang Batas Volume Tinggi**: Terjadi jika Z-Score Volume > 3.0. Sinyal yang disertai dengan lonjakan volume fantastis akan mendapatkan **bonus multiplier 1,3x**.

*(Catatan: Aturan jam malam atau filter pesanan mutlak telah dicabut karena merusak sinyal-sinyal probabilitas tinggi pada waktu tidak biasa.)*

---

## 2. Manajemen Risiko & Portofolio Virtual

Bekerja berdampingan dengan `PortfolioManager` (pada `app/portfolio_manager.go`), kini sistem memitigasi kerugian tidak hanya dengan persentase angka, tetapi dengan eksekusi ukuran lot virtual.

### a. Position Sizing (Penentuan Ukuran Lot)
- Menggunakan parameter `MAX_POSITION_PCT` (default 10%).
- Jika saldo virtual Rp200.000, maka setiap posisi *buy* maksimal memakan alokasi sebesar Rp20.000, berapa pun harga sahamnya.
- Saat IHSG sedang tidak stabil (`NEUTRAL`), ukuran lot yang boleh masuk akan dikompresi menjadi 70%.

### b. Biaya Transaksi Realistis (Fee)
Agar *win-rate* bukan sebatas angka manis, kriteria untuk mengklasifikasikan hasil sinyal menjadi `WIN` telah diubah:
- Ambang batas bukan lagi `0.0%`, melainkan **`0.25%`**. Hal ini untuk menutupi biaya sekuritas standar di Indonesia (0,15% beli, 0,10% jual).

### c. Pemutus Sirkuit Beruntun (*Circuit Breaker*)
Sistem memiliki pengaman di level aplikasi:
- **MaxDailyLossPct (Maksimum Kerugian Harian)**: Batas kerugian hingga `5.0%`.
- **MaxConsecutiveLosses (Maksimum Kerugian Beruntun)**: Sistem otomatis berhenti memproses aksi `BUY` jika mencapai 3x kerugian berturut-turut pada hari itu.

---

## 3. Strategi Keluar (*Exit Strategy*) yang Lebih Cerdas

Berada di `app/exit_strategy.go`, mekanisme ini dirancang untuk menjaga profit yang sudah susah payah didapatkan.

### a. Mekanisme Titik Impas (*Breakeven Mechanism*)
Daripada memasang limit secara persentase, sekarang bisa dikonfigurasi fleksibel:
- **BreakevenTriggerPct**: 1.0% (sistem mengaktifkan tameng perlindungan modal jika posisi surplus 1%).
- **BreakevenBufferPct**: 0.15% (menggeser stop loss asli Anda ke +0.15% di atas titik masuk/entry, mengunci keuntungan kecil yang menutupi *fee* sekuritas).

### b. Profit-Taking Berbasis Peluruhan Waktu (*Time-Decay*)
Saham yang diam tak bergerak akan kehilangan momentum.
- Jika setelah 2 jam harga tidak menyentuh Target Profit 1 (TP1), maka target TP1 tersebut diturunkan secara progresif sebesar 20%.
- Tujuannya agar sistem mencairkan (*liquidate*) posisi lebih awal pada perdagangan mandek.

### c. Perlindungan Pergeseran Stop Loss (*Trailing Stop Drift*)
- *Trailing Stop* diamankan ke *cache*. Ketika ATR dihitung ulang, stop loss hanya bisa bergerak **naik** (mengamankan profit) dan tidak akan pernah turun kembali.

---

## 📊 Hasil yang Diharapkan

1. **Win Rate Lebih Realistis**: Biaya layanan bursa (0,25%) disertakan, sehingga label kemenangan (`WIN`) jauh lebih bisa diandalkan.
2. **Kehancuran Modal yang Diminimalkan**: Konfigurasi portofolio dengan *circuit breaker* memastikan modal tak tersedot di kala pasar berantakan.
3. **Peluang Emas Lebih Tinggi**: Berubahnya blok pemblokiran statis menjadi sistem pengali (*multiplier*) mengurangi jumlah sinyal yang tertolak konyol tanpa perhitungan logika matematis.

👉 *Ingin mencobanya membiarkan aset Anda tumbuh semalaman tanpa pantauan harian?* Baca **[Panduan Swing Trading](SWING_TRADING.md)**.

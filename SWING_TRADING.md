# 🌙 Panduan Swing Trading

*Kembali ke [Halaman Utama](README.md)* | *Lihat [Logika Perbaikan Sinyal](SIGNAL_IMPROVEMENTS.md)*

## Gambaran Umum

Selain eksekusi kilat (Day Trading), sistem trading ini juga mendukung **Swing Trading**. Anda bisa menahan aset yang menjanjikan melintasi malam (*overnight*) hingga kurun waktu 30 hari (dapat disesuaikan) tanpa dicairkan secara paksa saat bursa tutup.

---

## ⚙️ Konfigurasi `.env`

Tambahkan variabel berikut ke dalam file `.env` Anda jika belum ada:

```env
# Swing Trading Configuration
SWING_TRADING_ENABLED=true          # Mengaktifkan mode Swing (default: false)
SWING_MIN_CONFIDENCE=0.75           # Kepercayaan minimum yang tinggi (default: 0.75)
SWING_MAX_HOLDING_DAYS=30           # Lama memegang posisi maksimal (default: 30 hari)
SWING_ATR_MULTIPLIER=3.0            # Pengali ATR untuk ruang bernapas volatilitas (default: 3.0)
SWING_MIN_BASELINE_DAYS=20          # Minimal harus ada data historis 20 hari
SWING_POSITION_SIZE_PCT=5.0         # Alokasi % saldo per posisi Swing
SWING_REQUIRE_TREND=true            # Mutlak butuh konfirmasi tren menguat
```

---

## ⚖️ Perbandingan Ekstrem: Day vs Swing

| Atribut Penentu | Day Trading ⚡ | Swing Trading 🌙 |
|-----------------|----------------|-------------------|
| **Durasi Pegang** | Maksimal 4 jam | Maksimal 30 hari |
| **Penutupan Pasar**| Tutup Paksa jam 16:00 WIB | Hold Overnight (dipertahankan) |
| **Stop Loss** | Sempit (1,5x ATR) | Lebar (4,5x ATR Harian) |
| **Take Profit 1** | 3x ATR | 9x ATR Harian |
| **Take Profit 2** | 6x ATR | 18x ATR Harian |
| **Minimal Baseline**| 50 data lilin (candles) | 400 data lilin (sekitar 20 hari) |
| **Level Keyakinan** | Standar (0,50) | Ekstra Tinggi (0,75) |

---

## 🔍 Empat Syarat Mutlak Menjadi "Swing Trade"

Setiap sinyal `BUY` yang muncul akan dites oleh sistem. Jika lolos empat uji di bawah, ia diizinkan masuk kandang Swing Trading:

1. **Keyakinan (*Confidence*) Premium**: Harus ≥ 0.75.
2. **Kekayaan Data Historis**: Harus memiliki data 20 hari terakhir. Jika tidak, perhitungan ATR Harian tidak akan presisi. **(Sistem Smart Bootstrap akan memfasilitasi penarikan ini saat inisialisasi awal)**.
3. **Kekuatan Tren Di Atas VWAP**: Posisi harga wajib di atas Volume-Weighted Average Price, dan skor tren ≥ 0.6.
4. **Swing Score Gabungan**:
   ```
   Swing Score = (Keyakinan × 0.4) + (Kekuatan Tren × 0.4) + (Lonjakan Volume × 0.2)
   ```
   *Skor akhir harus ≥ 0.65.*

---

## 🛡️ Strategi Exit untuk Mode Swing

### Penentuan Titik Menggunakan ATR Harian
Sistem menggunakan hitungan ATR Harian, yang artinya jarak *stop-loss* jauh lebih renggang (mengantisipasi gap harga pembukaan esok harinya).
```go
Stop Loss      = Daily ATR × 3.0 × 1.5  // 4,5x ruang toleransi kerugian
Trailing Stop  = Daily ATR × 3.0        // Mengunci profit pelan-pelan
Take Profit 1  = Daily ATR × 3.0 × 3.0  // Target 9x
Take Profit 2  = Daily ATR × 3.0 × 6.0  // Target Puncak 18x
```

### Mengapa Toleransi Stop-Loss Dilebarkan?
Hal ini sangat krusial agar aset tidak tersapu (*stop-out*) oleh kepanikan pagi hari (volatilitas pukul 09.00 - 09.15) saat pembukaan pasar BEI, sebelum akhirnya pasar merangkak naik lagi sepanjang hari.

---

## 🚦 Studi Kasus Skenario

### Kasus 1: Kelayakan Sempurna (Swing Mode)
Sebuah sinyal memicu pembelian saham `BBCA`:
- **Keyakinan**: 0.82
- **Posisi Harga**: 8% di atas VWAP
- **Volume**: 3,5x rata-rata
- **Skor Gabungan**: 0.72

👉 *Tindakan*: Dikategorikan sebagai **SWING TRADE**. Tidak ditutup otomatis pukul 16:00, dengan target profit yang jauh di awang (+15% / +30%).

### Kasus 2: Penolakan Menjadi Day Trade Saja
Sebuah sinyal memicu pembelian saham `GOTO`:
- **Keyakinan**: 0.58
- **Posisi Harga**: 2% di atas VWAP
- **Volume**: 2,1x rata-rata
- **Skor Gabungan**: 0.45 (Tidak lolos batas 0.65)

👉 *Tindakan*: Akan diperlakukan murni sebagai **DAY TRADE**. Batas *cut-loss* pendek (-1,5%) dan *Take Profit* kecil (+3%). Posisi akan dijual paksa pukul 16:00 WIB untuk mencegah risiko esok harinya.

---

## 🎯 Kapan Sebaiknya Menggunakan Mode Ini?

**Sangat Bagus Untuk:**
- Saham dengan kapitalisasi besar (Bluechips) yang sedang merangkak stabil.
- Kondisi Anda yang tidak bisa menatap layar monitor (*pantau intraday*) terus menerus.
- Sinyal yang disertai formasi mingguan (*weekly chart*) kuat.

**Sangat Buruk Untuk:**
- Saham gorengan dengan likuiditas rendah (Risiko tergelincir atau *slippage* parah saat buka pasar).
- Musim rilis laporan keuangan yang bisa memutarbalikkan nasib saham keesokan paginya (*Earnings Gap*).
- Indeks Harga Saham Gabungan (IHSG) sedang panik/Bearish. *(IHSG Safety Gate dari Python Screener biasanya akan mematikan ini secara otomatis)*.

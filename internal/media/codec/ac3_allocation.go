package codec

// ac3ZeroBAP вычисляет фактический BAP из экспонент и SNR канала channel и общего BAI allocation.
// Используется нормативный алгоритм A/52 §7.2 для fscod=0, двух FBW каналов без coupling/DBA.
// Все массивы имеют фиксированную вместимость; содержимое AU не задаёт размеры выделений.
func ac3ZeroBAP(channel ac3ZeroChannel, allocation ac3ZeroAllocation) [256]uint8 {
	// baps хранит итоговую точность квантования каждой спектральной позиции; SNR−960 требует все BAP=0.
	var baps [256]uint8
	if channel.snr == -960 {
		return baps
	}
	// psd содержит мощность каждой позиции в логарифмической шкале, bands — интегральную PSD полос.
	var psd [256]int
	var bands [50]int
	// bin выбирает фактическую экспоненту каждой используемой позиции.
	for bin := range channel.end {
		psd[bin] = 3072 - 128*channel.exponents[bin]
	}
	// bandCount считает используемые критические полосы, cursor указывает следующую суммируемую позицию.
	bandCount, cursor := 0, 0
	for cursor < channel.end {
		// power начинает интегрирование с первой позиции текущей полосы.
		power := psd[cursor]
		cursor++
		// end ограничивает последнюю неполную полосу фактической шириной спектра.
		end := min(ac3BandStart[bandCount+1], channel.end)
		for cursor < end {
			// peak и address выбирают нормативную поправку логарифмического сложения двух PSD.
			peak := max(power, psd[cursor])
			address := min(peak-((power+psd[cursor]+1)>>1), 255)
			power = peak + ac3LogAdd[address]
			cursor++
		}
		bands[bandCount] = power
		bandCount++
	}
	// excitation содержит распределённую PSD после низкочастотной компенсации и двух функций затухания.
	var excitation [50]int
	// lowcomp задаёт текущую низкочастотную компенсацию в шкале PSD.
	lowcomp := ac3LowComp(0, bands[0], bands[1], 384)
	excitation[0] = bands[0] - channel.fastGain - lowcomp
	lowcomp = ac3LowComp(lowcomp, bands[1], bands[2], 384)
	excitation[1] = bands[1] - channel.fastGain - lowcomp
	// begin выбирает полосу, с которой применяется распространяющаяся маска.
	begin := 7
	// fastLeak и slowLeak накапливают быстрое и медленное распространение PSD.
	fastLeak, slowLeak := 0, 0
	// band обрабатывает начальные низкочастотные полосы до первой неубывающей PSD.
	for band := 2; band < 7; band++ {
		lowcomp = ac3LowComp(lowcomp, bands[band], bands[band+1], 384)
		fastLeak, slowLeak = bands[band]-channel.fastGain, bands[band]-allocation.slowGain
		excitation[band] = fastLeak - lowcomp
		if bands[band] <= bands[band+1] {
			begin = band + 1
			break
		}
	}
	// band продолжает низкочастотную компенсацию до полосы 21 включительно.
	for band := begin; band < min(bandCount, 22); band++ {
		switch {
		case band < 7:
			lowcomp = ac3LowComp(lowcomp, bands[band], bands[band+1], 384)
		case band < 20:
			lowcomp = ac3LowComp(lowcomp, bands[band], bands[band+1], 320)
		default:
			lowcomp = max(lowcomp-128, 0)
		}
		fastLeak = max(fastLeak-allocation.fastDecay, bands[band]-channel.fastGain)
		slowLeak = max(slowLeak-allocation.slowDecay, bands[band]-allocation.slowGain)
		excitation[band] = max(fastLeak-lowcomp, slowLeak)
	}
	// band обрабатывает оставшиеся высокочастотные полосы без низкочастотной компенсации.
	for band := 22; band < bandCount; band++ {
		fastLeak = max(fastLeak-allocation.fastDecay, bands[band]-channel.fastGain)
		slowLeak = max(slowLeak-allocation.slowDecay, bands[band]-allocation.slowGain)
		excitation[band] = max(fastLeak, slowLeak)
	}
	// band выбирает общую маску для всех спектральных позиций данной критической полосы.
	for band := range bandCount {
		// mask объединяет возбуждение, поправку децибелов на бит и нормативный порог слышимости.
		mask := max(excitation[band]+max(allocation.dbPerBit-bands[band], 0)/4, ac3Hearing48[band])
		// adjusted вычитает SNR и нижний порог, затем квантует маску согласно A/52 §7.2.2.7.
		adjusted := (max(mask-channel.snr-allocation.floor, 0) & 0x1fe0) + allocation.floor
		// bin выбирает только используемые коэффициенты последней, возможно неполной полосы.
		for bin := ac3BandStart[band]; bin < min(ac3BandStart[band+1], channel.end); bin++ {
			// address ограничивает индекс нормативной таблицы BAP шестью битами.
			address := min(max((psd[bin]-adjusted)>>5, 0), 63)
			baps[bin] = ac3BAPTable[address]
		}
	}
	return baps
}

// ac3LowComp обновляет компенсацию value по соседним PSD left/right и нормативному уровню level.
// Рост PSD ровно на 256 включает level; спад уменьшает прежнюю компенсацию на 64.
func ac3LowComp(value, left, right, level int) int {
	if left+256 == right {
		return level
	}
	if left > right {
		return max(value-64, 0)
	}
	return value
}

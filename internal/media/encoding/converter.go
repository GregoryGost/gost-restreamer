package encoding

import (
	"fmt"
	"slices"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// SoftwareVideoFilter строит программное преобразование source в target по известным фактическим сведениям.
// Оба конца требуют достоверных CICP, формата пикселей, глубины и диапазона. HDR допускает только
// одинаковые transfer/range/depth10 и применимые YUV-плоскости. rescale разрешает изменение
// размеров, но не влияет на запрошенное преобразование цвета и глубины. FPS добавляет вызывающий.
// Серая Y поднимается через плоскости, чтобы limited range не изменился неявным swscale.
func SoftwareVideoFilter(source, target core.VideoFormat, rescale bool) (string, error) {
	if hdrTransfer(source.Color.Transfer) || hdrTransfer(target.Color.Transfer) {
		return hdrVideoFilter(source, target, rescale)
	}
	// err сообщает о неподдержанном исходном или целевом цветовом конце до запуска процесса.
	if err := converterEndpoint(source); err != nil {
		return "", fmt.Errorf("исходное видео: %w", err)
	}
	// err сообщает о неподдержанном целевом цветовом конце до запуска процесса.
	if err := converterEndpoint(target); err != nil {
		return "", fmt.Errorf("целевое видео: %w", err)
	}
	if !rescale && (source.Width != target.Width || source.Height != target.Height) {
		return "", fmt.Errorf("%w: rescale_output=false требует совпадения размеров", core.ErrInvalid)
	}
	// filter фиксирует фактический формат пикселей на входе без неявной смены диапазона.
	filter := "format=" + source.PixelFormat
	if source.PixelFormat == "gray" {
		filter += ",split=3[gy][gu][gv];[gu]lut=y=128,format=gray[u];[gv]lut=y=128,format=gray[v];[gy][u][v]mergeplanes=0x001020:format=yuv444p"
	}
	// pixel задаёт выходной YUV для zscale; отдельная Y извлекается после него для gray.
	pixel := target.PixelFormat
	if pixel == "gray" {
		pixel = "yuv444p"
	}
	filter += fmt.Sprintf(",zscale=w=%d:h=%d:primariesin=%d:transferin=%d:matrixin=%d:rangein=%s:primaries=%d:transfer=%d:matrix=%d:range=%s,format=%s", target.Width, target.Height,
		cicpPrimaries(source.Color.Primaries), cicpTransfer(source.Color.Transfer), cicpMatrix(source.Color.Matrix), converterRange(source.Color.Range),
		cicpPrimaries(target.Color.Primaries), cicpTransfer(target.Color.Transfer), cicpMatrix(target.Color.Matrix), converterRange(target.Color.Range), pixel)
	if target.PixelFormat == "gray" {
		filter += ",extractplanes=y"
	}
	filter += fmt.Sprintf(",setparams=range=%s:color_primaries=%d:color_trc=%d:colorspace=%d", converterRange(target.Color.Range), cicpPrimaries(target.Color.Primaries), cicpTransfer(target.Color.Transfer), cicpMatrix(target.Color.Matrix))
	return filter, nil
}

// hdrTransfer различает две поддержанные HDR-функции name; SDR-каталог конвертера не расширяется.
func hdrTransfer(name string) bool {
	return name == "smpte2084" || name == "arib-std-b67"
}

// hdrVideoFilter меняет только геометрию и YUV-плоскости source в target, сохраняя CICP, range и 10 бит.
// rescale запрещает несовпадающие размеры LIVE; FPS по-прежнему задаёт вызывающий.
func hdrVideoFilter(source, target core.VideoFormat, rescale bool) (string, error) {
	// endpoint обозначает очередной конец преобразования с обязательными фактическими HDR-сведениями.
	for _, endpoint := range []core.VideoFormat{source, target} {
		if endpoint.Width == 0 || endpoint.Height == 0 || endpoint.Width%2 != 0 || endpoint.Height%2 != 0 || endpoint.BitDepth != 10 ||
			!slices.Contains([]string{"yuv420p10le", "yuv422p10le", "yuv444p10le"}, endpoint.PixelFormat) ||
			endpoint.Color.Primaries != "bt2020" || endpoint.Color.Matrix != "bt2020nc" || !hdrTransfer(endpoint.Color.Transfer) ||
			!slices.Contains([]string{"limited", "full", "tv", "pc"}, endpoint.Color.Range) {
			return "", fmt.Errorf("%w: нужны известные HDR CICP, range и 10-bit YUV", core.ErrInvalid)
		}
	}
	if source.Color.Transfer != target.Color.Transfer || converterRange(source.Color.Range) != converterRange(target.Color.Range) ||
		!rescale && (source.Width != target.Width || source.Height != target.Height) {
		return "", fmt.Errorf("%w: преобразование HDR-функции, диапазона или запрещённых размеров", core.ErrInvalid)
	}
	// transfer задаёт точный CICP PQ либо HLG, не меняя саму передаточную функцию.
	transfer := 16
	if target.Color.Transfer == "arib-std-b67" {
		transfer = 18
	}
	return fmt.Sprintf("format=%s,zscale=w=%d:h=%d:primariesin=9:transferin=%d:matrixin=9:rangein=%s:primaries=9:transfer=%d:matrix=9:range=%s,format=%s,setparams=range=%s:color_primaries=9:color_trc=%d:colorspace=9",
		source.PixelFormat, target.Width, target.Height, transfer, converterRange(source.Color.Range), transfer, converterRange(target.Color.Range), target.PixelFormat, converterRange(target.Color.Range), transfer), nil
}

// converterEndpoint проверяет конечную область программного zscale без подстановки отсутствующих
// метаданных. v может иметь любой распознанный профиль исходного кодека; важны фактические плоскости.
func converterEndpoint(v core.VideoFormat) error {
	if v.Width == 0 || v.Height == 0 || v.Width%2 != 0 || v.Height%2 != 0 ||
		!slices.Contains([]string{"limited", "full", "tv", "pc"}, v.Color.Range) ||
		cicpPrimaries(v.Color.Primaries) == 0 || cicpTransfer(v.Color.Transfer) == 0 || cicpMatrix(v.Color.Matrix) == 0 {
		return fmt.Errorf("%w: нужны известные размеры, диапазон и SDR CICP", core.ErrInvalid)
	}
	if !slices.Contains([]string{"yuv420p", "yuv420p10le", "yuv422p", "yuv422p10le", "yuv444p", "yuv444p10le", "gray"}, v.PixelFormat) ||
		strings.HasSuffix(v.PixelFormat, "10le") && v.BitDepth != 10 || !strings.HasSuffix(v.PixelFormat, "10le") && v.BitDepth != 8 {
		return fmt.Errorf("%w: неподдержанные плоскости или глубина", core.ErrInvalid)
	}
	if v.Color.Matrix == "bt2020c" && (v.PixelFormat == "gray" || !slices.Contains([]string{"bt709", "smpte170m", "bt2020-10", "bt2020-12"}, v.Color.Transfer)) {
		return fmt.Errorf("%w: несовместимые matrix/transfer/gray", core.ErrInvalid)
	}
	return nil
}

// cicpPrimaries возвращает нормативный номер только для поддержанных основных цветов name.
func cicpPrimaries(name string) int {
	return map[string]int{"bt709": 1, "bt470m": 4, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7, "film": 8, "bt2020": 9}[name]
}

// cicpTransfer возвращает нормативный номер поддержанной передаточной функции name.
// Имена ffprobe bt470m/bg означают transfer 4/5; их примесь не меняет отдельную таблицу primaries.
func cicpTransfer(name string) int {
	return map[string]int{"bt709": 1, "gamma22": 4, "bt470m": 4, "gamma28": 5, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7, "linear": 8, "iec61966-2-1": 13, "bt2020-10": 14, "bt2020-12": 15}[name]
}

// cicpMatrix возвращает нормативный номер только для поддержанной матрицы name.
func cicpMatrix(name string) int {
	return map[string]int{"bt709": 1, "fcc": 4, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7, "bt2020nc": 9, "bt2020c": 10}[name]
}

// converterRange переводит проверенный name в имя диапазона zscale и setparams.
func converterRange(name string) string {
	if name == "pc" || name == "full" {
		return "full"
	}
	return "limited"
}

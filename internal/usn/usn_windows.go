//go:build windows

// Package usn 直接读 NTFS 的 USN Journal（Change Journal），
// 不遍历目录就能拿到"这个卷上有哪些文件"。Everything 的枚举层就是这么做的。
//
// 两条原语：
//   - FSCTL_ENUM_USN_DATA：按 FileReferenceNumber 顺序吐出整个卷的 $MFT 记录，
//     每条给你 FRN / 父 FRN / 名字 / 属性。179 万条实测几秒，比 WalkDir 快一个量级。
//   - FSCTL_READ_USN_JOURNAL：从某个 USN 位点开始读增量记录，
//     用来知道"卷上又变了什么"（建/删/改名）。
//
// 代价：打开卷句柄并要求 SE_MANAGE_VOLUME，所以只能在提权进程里跑（服务或管理员）。
// USN 记录没有大小和时间（那在 $MFT 的 $STANDARD_INFORMATION 属性里），
// 所以由 USN 建出来的索引 Size/Mod 是 0——搜索排序不依赖这两项，界面上也不显示。
package usn

import (
	"encoding/binary"
	"fmt"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fsctlQueryUsnJournal = 0x000900f4
	fsctlEnumUsnData     = 0x000900b3
	fsctlReadUsnJournal  = 0x000900bb

	// 根目录在 NTFS 里的 FileReferenceNumber 恒为 5。
	RootFRN uint64 = 5

	enumBufSize = 1 << 20
	readBufSize = 1 << 20
)

// 记录属性位
const (
	AttrHidden       = 0x0002
	AttrSystem       = 0x0004
	AttrDirectory    = 0x0010
	AttrReparsePoint = 0x0400
)

// Record 是 USN_RECORD_V2 的一条（我们只认 V2，卷上绝大多数记录都是它）。
type Record struct {
	FRN       uint64
	ParentFRN uint64
	USN       int64
	Reason    uint32
	Attrs     uint32
	Name      string
}

// IsDir 判断这条记录是不是目录。
func (r *Record) IsDir() bool { return r.Attrs&AttrDirectory != 0 }

// Journal 是卷上 USN Journal 的当前位置与身份。
type Journal struct {
	ID       uint64
	FirstUsn int64
	NextUsn  int64
}

// Volume 是一个打开的卷句柄。
type Volume struct {
	h      windows.Handle
	letter string
}

type mftEnumData struct {
	StartFRN uint64
	LowUsn   int64
	HighUsn  int64
}

type readReq struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
}

type journalData struct {
	ID          uint64
	FirstUsn    int64
	NextUsn     int64
	LowestValid int64
	MaxUsn      int64
	MaxSize     uint64
	AllocDelta  uint64
	// V1 还多两个字段，多给点空间不影响
	_ [16]byte
}

// Open 打开一个卷。drive 是盘符字母，例如 "C"。
func Open(drive string) (*Volume, error) {
	if len(drive) != 1 {
		return nil, fmt.Errorf("盘符无效: %q", drive)
	}
	path := `\\.\` + drive + `:`
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("打开卷 %s: %w", path, err)
	}
	return &Volume{h: h, letter: drive}, nil
}

// Close 关闭卷句柄。
func (v *Volume) Close() error { return windows.CloseHandle(v.h) }

// Letter 返回盘符字母。
func (v *Volume) Letter() string { return v.letter }

// Journal 查询当前 USN Journal 状态。
func (v *Volume) Journal() (Journal, error) {
	var out journalData
	var returned uint32
	err := windows.DeviceIoControl(v.h, fsctlQueryUsnJournal,
		nil, 0,
		(*byte)(unsafe.Pointer(&out)), uint32(unsafe.Sizeof(out)),
		&returned, nil)
	if err != nil {
		return Journal{}, fmt.Errorf("查询 USN Journal（%s:）: %w", v.letter, err)
	}
	return Journal{ID: out.ID, FirstUsn: out.FirstUsn, NextUsn: out.NextUsn}, nil
}

// Enum 全量枚举卷上的所有记录（按 FRN 递增）。
// fn 返回 false 表示提前停止。
func (v *Volume) Enum(fn func(*Record) bool) error {
	next, err := v.Journal()
	if err != nil {
		return err
	}
	return v.enumUntil(next.NextUsn, fn)
}

// enumUntil 枚举到 highUsn 为止的记录。
func (v *Volume) enumUntil(highUsn int64, fn func(*Record) bool) error {
	in := mftEnumData{StartFRN: 0, LowUsn: 0, HighUsn: highUsn}
	buf := make([]byte, enumBufSize)
	for {
		var returned uint32
		err := windows.DeviceIoControl(v.h, fsctlEnumUsnData,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			&buf[0], uint32(len(buf)),
			&returned, nil)
		if err == windows.ERROR_HANDLE_EOF || err == windows.ERROR_NO_MORE_FILES {
			return nil
		}
		if err != nil {
			return fmt.Errorf("枚举卷 %s: %w", v.letter, err)
		}
		if returned < 8 {
			return nil
		}
		in.StartFRN = binary.LittleEndian.Uint64(buf[:8])
		for off := 8; off+60 <= int(returned); {
			rlen := int(binary.LittleEndian.Uint32(buf[off:]))
			if rlen < 60 || off+rlen > int(returned) {
				return nil // 尾部残缺，下一轮 IO 会重新给
			}
			rec, ok := parseRecord(buf[off : off+rlen])
			if ok && !fn(rec) {
				return nil
			}
			off += rlen
		}
		if in.StartFRN == 0 {
			return nil
		}
	}
}

// Read 从 startUsn 开始读增量记录。wait<=0 时立即返回（只拿已经落盘的记录）。
// 返回读到的下一位点与条数。
func (v *Volume) Read(startUsn int64, journalID uint64, wait time.Duration, fn func(*Record) bool) (int64, int, error) {
	secs := uint64(0)
	if wait > 0 {
		secs = uint64(wait / time.Second)
		if secs == 0 {
			secs = 1
		}
	}
	in := readReq{
		StartUsn: startUsn,
		// 0xFFFFFFFF 表示不筛 reason；ReturnOnlyOnClose=1 让每条文件只在关闭时出一次记录，
		// 避免同一个文件在一次写入里出几十条中间记录。
		ReasonMask:        0xFFFFFFFF,
		ReturnOnlyOnClose: 1,
		Timeout:           secs,
		BytesToWaitFor:    1,
		UsnJournalID:      journalID,
	}
	buf := make([]byte, readBufSize)
	var returned uint32
	err := windows.DeviceIoControl(v.h, fsctlReadUsnJournal,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		&buf[0], uint32(len(buf)),
		&returned, nil)
	if err == windows.ERROR_HANDLE_EOF || err == windows.ERROR_NO_MORE_FILES {
		return startUsn, 0, nil
	}
	if err != nil {
		return startUsn, 0, fmt.Errorf("读 USN Journal（%s:）: %w", v.letter, err)
	}
	if returned < 8 {
		return startUsn, 0, nil
	}
	nextUsn := int64(binary.LittleEndian.Uint64(buf[:8]))
	count := 0
	for off := 8; off+60 <= int(returned); {
		rlen := int(binary.LittleEndian.Uint32(buf[off:]))
		if rlen < 60 || off+rlen > int(returned) {
			break
		}
		if rec, ok := parseRecord(buf[off : off+rlen]); ok {
			count++
			if !fn(rec) {
				break
			}
		}
		off += rlen
	}
	return nextUsn, count, nil
}

// parseRecord 解析一段 USN_RECORD（只接受 MajorVersion==2）。
//
// V2 布局：RecordLength u32 | Major u16 | Minor u16 | FRN u64 | ParentFRN u64 |
// Usn i64 | TimeStamp i64 | Reason u32 | SourceInfo u32 | SecurityId u32 |
// FileAttributes u32 | NameLength u16 | NameOffset u16 | Name UTF-16
func parseRecord(b []byte) (*Record, bool) {
	if len(b) < 60 {
		return nil, false
	}
	if binary.LittleEndian.Uint16(b[4:]) != 2 {
		return nil, false
	}
	nameLen := int(binary.LittleEndian.Uint16(b[56:]))
	nameOff := int(binary.LittleEndian.Uint16(b[58:]))
	if nameOff < 60 || nameOff+nameLen > len(b) || nameLen < 2 {
		return nil, false
	}
	u16 := make([]uint16, nameLen/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(b[nameOff+i*2:])
	}
	return &Record{
		FRN:       binary.LittleEndian.Uint64(b[8:]),
		ParentFRN: binary.LittleEndian.Uint64(b[16:]),
		USN:       int64(binary.LittleEndian.Uint64(b[24:])),
		Reason:    binary.LittleEndian.Uint32(b[40:]),
		Attrs:     binary.LittleEndian.Uint32(b[52:]),
		Name:      string(utf16.Decode(u16)),
	}, true
}

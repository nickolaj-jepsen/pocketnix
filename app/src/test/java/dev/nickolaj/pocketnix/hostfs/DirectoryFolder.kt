package dev.nickolaj.pocketnix.hostfs

import dev.nickolaj.pocketnix.agent.GuestFile
import java.io.File
import java.io.FileNotFoundException
import java.nio.ByteBuffer
import java.nio.channels.FileChannel
import java.nio.file.FileAlreadyExistsException
import java.nio.file.Files
import java.nio.file.StandardOpenOption

/** A plain directory as a shared folder, standing in for an Android one. */
class DirectoryFolder(private val root: File) : HostFolderFs {
    private fun file(path: String) = if (path.isEmpty()) root else File(root, path)

    private fun info(path: String, f: File): GuestFile {
        if (!f.exists()) throw FileNotFoundException(path)
        return GuestFile(path, if (path.isEmpty()) "" else f.name, f.isDirectory, if (f.isDirectory) 0 else f.length(), f.lastModified(), f.canWrite())
    }

    override fun stat(path: String) = info(path, file(path))

    override fun list(path: String): List<GuestFile> {
        val dir = file(path)
        if (!dir.isDirectory) throw FileNotFoundException(path)
        return dir.listFiles()!!.map { info(if (path.isEmpty()) it.name else "$path/${it.name}", it) }
    }

    override fun create(dir: String, name: String, isDir: Boolean): GuestFile {
        val p = if (dir.isEmpty()) name else "$dir/$name"
        val f = file(p)
        if (f.exists()) throw FileAlreadyExistsException(p)
        if (isDir) Files.createDirectory(f.toPath()) else Files.createFile(f.toPath())
        return stat(p)
    }

    override fun delete(path: String) {
        val f = file(path)
        if (!f.exists()) throw FileNotFoundException(path)
        f.deleteRecursively()
    }

    override fun rename(path: String, to: String): GuestFile {
        if (file(to).exists()) throw FileAlreadyExistsException(to)
        Files.move(file(path).toPath(), file(to).toPath())
        return stat(to)
    }

    override fun open(path: String, mode: String): HostFile {
        val f = file(path)
        if (!f.isFile) throw FileNotFoundException(path)
        val opts = buildSet {
            add(StandardOpenOption.READ)
            if (mode != "r") add(StandardOpenOption.WRITE)
            if (mode == "rwt") add(StandardOpenOption.TRUNCATE_EXISTING)
        }
        val ch = FileChannel.open(f.toPath(), opts)
        return object : HostFile {
            override fun read(offset: Long, buf: ByteArray, len: Int): Int {
                val b = ByteBuffer.wrap(buf, 0, len)
                while (b.hasRemaining() && ch.read(b, offset + b.position()) > 0) Unit
                return b.position()
            }

            override fun write(offset: Long, data: ByteArray, off: Int, len: Int) {
                val b = ByteBuffer.wrap(data, off, len)
                while (b.hasRemaining()) ch.write(b, offset + b.position() - off)
            }

            override fun truncate(size: Long) {
                if (size < ch.size()) ch.truncate(size) else if (size > ch.size()) ch.write(ByteBuffer.wrap(ByteArray(1)), size - 1)
            }

            override fun sync() = ch.force(true)

            override fun close() = ch.close()
        }
    }
}

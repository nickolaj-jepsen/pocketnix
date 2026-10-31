package dev.nickolaj.pocketnix.ui

/** A disk size typed into a form, checked by [checkDiskSize]. */
sealed interface DiskSize {
    data class Ok(val gb: Int) : DiskSize

    /** Not a whole number of GB from the minimum to the free space. */
    data object OutOfRange : DiskSize

    /** The free space is below the minimum: no size fits, whatever is typed. */
    data object NoRoom : DiskSize
}

/** Checks [text] as a disk size from [minGb] to [maxGb], the phone's free space for the disk. */
fun checkDiskSize(text: String, minGb: Int, maxGb: Int): DiskSize = when {
    maxGb < minGb -> DiskSize.NoRoom
    else -> text.toIntOrNull()?.takeIf { it in minGb..maxGb }?.let { DiskSize.Ok(it) } ?: DiskSize.OutOfRange
}
